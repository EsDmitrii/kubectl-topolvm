// Package lvsdiff compares the LogicalVolume objects of a node with what lvs prints on it. The
// plugin never runs anything on a node: the user captures lvs and passes the output in.
package lvsdiff

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

// States, most serious first.
const (
	DiskOnly     = "disk-only"
	CROnly       = "cr-only"
	SizeMismatch = "size-mismatch"
	// OtherNode is an LV whose LogicalVolume the API places on another node: the lvs output most
	// likely comes from that node.
	OtherNode = "other-node"
	OK        = "ok"
	Foreign   = "foreign"
)

var stateOrder = map[string]int{OtherNode: 0, DiskOnly: 1, CROnly: 2, SizeMismatch: 3, OK: 4, Foreign: 5}

// TopoLVM names LVs on disk by the LogicalVolume uid.
var uuidName = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Row is one LV seen on disk, in the API, or both.
type Row struct {
	State     string `json:"state"`
	LV        string `json:"lv"`
	VG        string `json:"vg"`
	DiskBytes *int64 `json:"diskBytes"`
	CRBytes   *int64 `json:"crBytes"`
	// CRNode is the node of the LogicalVolume with this volume ID, null when there is none.
	CRNode *string `json:"crNode"`
	PVC    string  `json:"pvc"`
}

type lvsReport struct {
	Report []struct {
		LV []struct {
			Name string `json:"lv_name"`
			VG   string `json:"vg_name"`
			Size string `json:"lv_size"`
		} `json:"lv"`
	} `json:"report"`
}

// Command is what the user runs on the node to produce the input. kubectl debug attaches only
// after the container starts, so lvs waits a moment or its output never reaches the terminal.
func Command(node string) string {
	return fmt.Sprintf("kubectl debug node/%s -i --quiet --profile=sysadmin --image=busybox -- "+
		"chroot /host sh -c 'sleep 2; lvs --reportformat json -o lv_name,vg_name,lv_size --units b --nosuffix' > lvs-%s.json", node, node)
}

// Diff matches disk LVs to LogicalVolumes of the node by volume ID. LVs in volume groups no
// TopoLVM device class uses, or not named like a volume ID, are foreign and shown only with all.
func Diff(in report.Input, node string, lvs io.Reader, all bool) ([]Row, error) {
	var rep lvsReport
	if err := json.NewDecoder(lvs).Decode(&rep); err != nil {
		return nil, fmt.Errorf("cannot read lvs JSON (run it with --reportformat json): %w", err)
	}
	vgs := map[string]bool{}
	classes := in.Classes
	if c, ok := in.NodeClasses[node]; ok {
		classes = c
	}
	for _, dc := range classes {
		if dc.VolumeGroup != "" {
			vgs[dc.VolumeGroup] = true
		}
	}
	claims := map[string]string{}
	for i := range in.PVs {
		pv := &in.PVs[i]
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == in.Driver.Name && pv.Spec.ClaimRef != nil {
			claims[pv.Spec.CSI.VolumeHandle] = claimName(pv)
		}
	}
	crs, elsewhere := map[string]int{}, map[string]int{}
	for i := range in.LVs {
		switch {
		case in.LVs[i].VolumeID == "":
		case in.LVs[i].NodeName == node:
			crs[in.LVs[i].VolumeID] = i
		default:
			elsewhere[in.LVs[i].VolumeID] = i
		}
	}

	rows := []Row{}
	seen := map[string]bool{}
	for _, r := range rep.Report {
		for _, d := range r.LV {
			size, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(d.Size), "B"), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("lv %s/%s: size %q: run lvs with --units b --nosuffix", d.VG, d.Name, d.Size)
			}
			row := Row{LV: d.Name, VG: d.VG, DiskBytes: new(size), PVC: "-"}
			idx, inAPI := crs[d.Name]
			far, onOther := elsewhere[d.Name]
			switch {
			case (len(vgs) > 0 && !vgs[d.VG]) || !uuidName.MatchString(d.Name):
				row.State = Foreign
			case onOther && !inAPI:
				row.State = OtherNode
				row.CRBytes = new(in.LVs[far].UsedBytes())
				row.CRNode = new(in.LVs[far].NodeName)
			case !inAPI:
				row.State = DiskOnly
			default:
				seen[d.Name] = true
				cr := in.LVs[idx].UsedBytes()
				row.CRBytes = &cr
				row.CRNode = new(node)
				row.State = OK
				if size < cr {
					row.State = SizeMismatch
				}
			}
			if c, ok := claims[d.Name]; ok {
				row.PVC = c
			}
			if row.State != Foreign || all {
				rows = append(rows, row)
			}
		}
	}
	for id, idx := range crs {
		if seen[id] {
			continue
		}
		cr := in.LVs[idx].UsedBytes()
		row := Row{State: CROnly, LV: id, VG: "-", CRBytes: &cr, CRNode: new(node), PVC: "-"}
		if c, ok := claims[id]; ok {
			row.PVC = c
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if stateOrder[rows[i].State] != stateOrder[rows[j].State] {
			return stateOrder[rows[i].State] < stateOrder[rows[j].State]
		}
		if rows[i].LV != rows[j].LV {
			return rows[i].LV < rows[j].LV
		}
		return rows[i].VG < rows[j].VG
	})
	return rows, nil
}

func claimName(pv *corev1.PersistentVolume) string {
	return pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
}

// Warnings flag output that probably comes from another node than the one named.
func Warnings(rows []Row, node string) []string {
	var warnings []string
	var others []string
	count := map[string]int{}
	for _, r := range rows {
		count[r.State]++
		if r.State == OtherNode && !slices.Contains(others, *r.CRNode) {
			others = append(others, *r.CRNode)
		}
	}
	if n := count[OtherNode]; n > 0 {
		noun := "LVs"
		if n == 1 {
			noun = "LV"
		}
		sort.Strings(others)
		warnings = append(warnings, fmt.Sprintf("lvs shows %d %s that the API places on other nodes (%s); was it captured on %s?",
			n, noun, strings.Join(others, ", "), node))
	}
	if count[OK]+count[SizeMismatch] == 0 && count[CROnly] > 0 && count[DiskOnly]+count[OtherNode] > 0 {
		warnings = append(warnings, fmt.Sprintf("no LV on disk matches a LogicalVolume of %s; was lvs captured on %s?", node, node))
	}
	return warnings
}

// Problems reports whether any row needs attention.
func Problems(rows []Row) bool {
	for _, r := range rows {
		if r.State == DiskOnly || r.State == CROnly || r.State == SizeMismatch || r.State == OtherNode {
			return true
		}
	}
	return false
}
