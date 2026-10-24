package report

import (
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// FitOptions describe the volume to place.
type FitOptions struct {
	Size         int64
	StorageClass string
	DeviceClass  string
	Replicas     int
	Nodes        []string
}

// FitRow is one candidate node.
type FitRow struct {
	Node           string   `json:"node"`
	DeviceClass    string   `json:"deviceClass"`
	Type           string   `json:"type"`
	FreeBytes      int64    `json:"freeBytes"`
	FreeAfterBytes *int64   `json:"freeAfterBytes"`
	Fits           bool     `json:"fits"`
	Count          int64    `json:"count"`
	Notes          []string `json:"notes"`
}

// FitResult answers where a volume of a given size fits.
type FitResult struct {
	SizeBytes    int64    `json:"sizeBytes"`
	Rows         []FitRow `json:"rows"`
	NodesThatFit int      `json:"nodesThatFit"`
	Replicas     int      `json:"replicas"`
	OK           bool     `json:"ok"`
}

var errNoStorageClasses = errors.New("storageclasses were not collected")

func findStorageClass(in *Input, name string) (*storagev1.StorageClass, error) {
	if in.StorageClasses == nil {
		return nil, errNoStorageClasses
	}
	for i := range in.StorageClasses {
		if in.StorageClasses[i].Name == name {
			return &in.StorageClasses[i], nil
		}
	}
	return nil, fmt.Errorf("storageclass %q not found", name)
}

// nodeNotes lists why the scheduler may skip a node even when it has room.
func nodeNotes(in *Input, node string) []string {
	notes := []string{}
	s, ok := in.NodeStates[node]
	if !ok {
		return notes
	}
	if s.Unschedulable {
		notes = append(notes, "cordoned")
	}
	if !s.Ready {
		notes = append(notes, "NotReady")
	}
	for _, t := range s.Taints {
		if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		kv := t.Key
		if t.Value != "" {
			kv += "=" + t.Value
		}
		notes = append(notes, fmt.Sprintf("taint %s:%s", kv, t.Effect))
	}
	return notes
}

// BuildFit lists the nodes that can host a new volume of the given size.
func BuildFit(in Input, o FitOptions) (FitResult, error) {
	class := o.DeviceClass
	if o.StorageClass != "" {
		sc, err := findStorageClass(&in, o.StorageClass)
		if err != nil {
			return FitResult{}, err
		}
		if sc.Provisioner != in.Driver.Name {
			return FitResult{}, fmt.Errorf("storageclass %q is not provisioned by %s", o.StorageClass, in.Driver.Name)
		}
		class = sc.Parameters[in.Driver.DeviceClassParameter()]
	}
	if o.Replicas < 1 {
		o.Replicas = 1
	}
	typeOf := classTypes(&in)
	res := FitResult{SizeBytes: o.Size, Rows: []FitRow{}, Replicas: o.Replicas}
	nodeFilter := set(o.Nodes)
	for _, n := range in.Nodes {
		if !allowed(nodeFilter, n.Name) {
			continue
		}
		c := class
		if c == "" {
			def, ok := in.defaultOf(n)
			if !ok {
				continue
			}
			c = def
		}
		free, ok := n.Capacity[c]
		if !ok {
			continue
		}
		row := FitRow{Node: n.Name, DeviceClass: c, Type: typeOf(n.Name, c), FreeBytes: free, Notes: nodeNotes(&in, n.Name)}
		if o.Size > 0 {
			row.Count = free / o.Size
		}
		row.Fits = row.Count >= 1
		if row.Fits {
			row.FreeAfterBytes = new(free - o.Size)
		}
		if row.Type == topolvm.TypeThin {
			row.Notes = append(row.Notes, "thin: virtual")
		}
		if row.Fits && schedulable(&in, n.Name) {
			res.NodesThatFit++
		}
		res.Rows = append(res.Rows, row)
	}
	sort.SliceStable(res.Rows, func(i, j int) bool {
		if res.Rows[i].FreeBytes != res.Rows[j].FreeBytes {
			return res.Rows[i].FreeBytes > res.Rows[j].FreeBytes
		}
		return res.Rows[i].Node < res.Rows[j].Node
	})
	res.OK = res.NodesThatFit >= o.Replicas
	return res, nil
}

func schedulable(in *Input, node string) bool {
	s, ok := in.NodeStates[node]
	return !ok || (s.Ready && !s.Unschedulable)
}
