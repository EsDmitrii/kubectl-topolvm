package report

import (
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// Volume kinds.
const (
	KindVolume   = "volume"
	KindSnapshot = "snapshot"
	KindClone    = "clone"
)

// StatusDeleting marks a volume whose LogicalVolume has a deletion timestamp.
const StatusDeleting = "Deleting"

// Volume is one LogicalVolume joined to its PersistentVolume and claim.
type Volume struct {
	LV               string    `json:"lv"`
	VolumeID         string    `json:"volumeID"`
	Node             string    `json:"node"`
	DeviceClass      string    `json:"deviceClass"`
	Kind             string    `json:"kind"`
	Source           *string   `json:"source"`
	PV               *string   `json:"pv"`
	Namespace        *string   `json:"namespace"`
	PVC              *string   `json:"pvc"`
	StorageClass     *string   `json:"storageClass"`
	ReclaimPolicy    *string   `json:"reclaimPolicy"`
	SizeBytes        int64     `json:"sizeBytes"`
	CurrentSizeBytes *int64    `json:"currentSizeBytes"`
	Status           string    `json:"status"`
	Code             int64     `json:"code"`
	Message          *string   `json:"message"`
	CreatedAt        time.Time `json:"createdAt"`

	// Filled only with --usage.
	FSUsedBytes     *int64 `json:"fsUsedBytes"`
	FSCapacityBytes *int64 `json:"fsCapacityBytes"`
	IdleBytes       *int64 `json:"idleBytes"`
}

// pvIndex finds the PersistentVolume of an LV by volumeHandle, the only way TopoLVM resolves a
// volume: a PV that shares the LV's name but not its volume ID cannot be mounted.
type pvIndex struct {
	byHandle map[string]*corev1.PersistentVolume
}

func newPVIndex(in *Input) pvIndex {
	x := pvIndex{byHandle: map[string]*corev1.PersistentVolume{}}
	for i := range in.PVs {
		pv := &in.PVs[i]
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == in.Driver.Name {
			x.byHandle[pv.Spec.CSI.VolumeHandle] = pv
		}
	}
	return x
}

func (x pvIndex) find(lv *topolvm.LogicalVolume) *corev1.PersistentVolume {
	return x.byHandle[lv.Key()]
}

// classResolver maps an LV to its device class using the default class of its node.
func classResolver(in *Input) func(*topolvm.LogicalVolume) string {
	type def struct {
		name string
		ok   bool
	}
	defaults := map[string]def{}
	for _, n := range in.Nodes {
		d, ok := in.defaultOf(n)
		defaults[n.Name] = def{d, ok}
	}
	absent, absentOK := defaultClass(topolvm.Node{}, in.Classes, nil)
	return func(lv *topolvm.LogicalVolume) string {
		d, known := defaults[lv.NodeName]
		if !known {
			d = def{absent, absentOK}
		}
		return resolveClass(lv, d.name, d.ok)
	}
}

func kindOf(lv *topolvm.LogicalVolume) string {
	switch {
	case lv.IsSnapshot():
		return KindSnapshot
	case lv.Source != "":
		return KindClone
	default:
		return KindVolume
	}
}

func statusOf(lv *topolvm.LogicalVolume, pv *corev1.PersistentVolume, pvsCollected bool) string {
	switch {
	case lv.Code != 0:
		return "Error"
	case lv.DeletionTimestamp != nil:
		return StatusDeleting
	case lv.CurrentSizeBytes != nil && *lv.CurrentSizeBytes < lv.SizeBytes:
		return "Resizing"
	case lv.IsSnapshot():
		return "Snapshot"
	case pv != nil:
		return string(pv.Status.Phase)
	case pvsCollected:
		return "Orphan"
	default:
		return unknownStatus
	}
}

const unknownStatus = "<unknown>"

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func buildVolumes(in Input, o Options) []Volume {
	nodeFilter, classFilter := set(o.Nodes), set(o.DeviceClasses)
	pvs := newPVIndex(&in)
	classOf := classResolver(&in)

	out := []Volume{}
	for i := range in.LVs {
		lv := &in.LVs[i]
		class := classOf(lv)
		if !allowed(nodeFilter, lv.NodeName) || !allowed(classFilter, class) {
			continue
		}
		pv := pvs.find(lv)
		v := Volume{
			LV:               lv.Name,
			VolumeID:         lv.VolumeID,
			Node:             lv.NodeName,
			DeviceClass:      class,
			Kind:             kindOf(lv),
			Source:           optional(lv.Source),
			SizeBytes:        lv.SizeBytes,
			CurrentSizeBytes: lv.CurrentSizeBytes,
			Status:           statusOf(lv, pv, in.PVs != nil),
			Code:             lv.Code,
			Message:          optional(lv.Message),
			CreatedAt:        lv.Created,
		}
		if pv != nil {
			v.PV = optional(pv.Name)
			v.StorageClass = optional(pv.Spec.StorageClassName)
			v.ReclaimPolicy = optional(string(pv.Spec.PersistentVolumeReclaimPolicy))
			if ref := pv.Spec.ClaimRef; ref != nil {
				v.Namespace, v.PVC = optional(ref.Namespace), optional(ref.Name)
				if u, ok := in.Usage[ref.Namespace+"/"+ref.Name]; ok {
					v.FSUsedBytes, v.FSCapacityBytes = u.UsedBytes, u.CapacityBytes
					if u.UsedBytes != nil {
						v.IdleBytes = new(lv.UsedBytes() - *u.UsedBytes)
					}
				}
			}
		}
		if o.Namespace != "" && (v.Namespace == nil || *v.Namespace != o.Namespace) {
			continue
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if an, bn := last(a.Namespace), last(b.Namespace); an != bn {
			return an < bn
		}
		if ap, bp := last(a.PVC), last(b.PVC); ap != bp {
			return ap < bp
		}
		return a.LV < b.LV
	})
	return out
}

// last sorts missing values after present ones.
func last(p *string) string {
	if p == nil {
		return "￿"
	}
	return *p
}
