package report

import (
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// Headroom says how far one claim can grow on its node.
type Headroom struct {
	Namespace         string            `json:"namespace"`
	PVC               string            `json:"pvc"`
	PV                string            `json:"pv"`
	LV                string            `json:"lv"`
	Node              string            `json:"node"`
	DeviceClass       string            `json:"deviceClass"`
	Type              string            `json:"type"`
	StorageClass      string            `json:"storageClass"`
	CurrentBytes      int64             `json:"currentBytes"`
	FreeBytes         *int64            `json:"freeBytes"`
	MaxBytes          *int64            `json:"maxBytes"`
	RequestedBytes    *int64            `json:"requestedBytes"`
	Fits              *bool             `json:"fits"`
	AllowExpansion    bool              `json:"allowExpansion"`
	Autoresizer       map[string]string `json:"autoresizer"`
	LastResizeFailure string            `json:"lastResizeFailure"`
}

// autoresizerPrefix marks pvc-autoresizer annotations on claims and StorageClasses.
const autoresizerPrefix = "resize.topolvm.io/"

func findPVC(in *Input, namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	if in.PVCs == nil {
		return nil, errors.New("persistentvolumeclaims were not collected")
	}
	for i := range in.PVCs {
		if in.PVCs[i].Namespace == namespace && in.PVCs[i].Name == name {
			return &in.PVCs[i], nil
		}
	}
	return nil, fmt.Errorf("persistentvolumeclaim %s/%s not found", namespace, name)
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}

// lastEvent is the message of the newest event with the reason for the object.
func lastEvent(in *Input, kind, namespace, name, reason string) string {
	var best *corev1.Event
	for i := range in.Events {
		e := &in.Events[i]
		if e.Reason != reason || e.InvolvedObject.Kind != kind || e.InvolvedObject.Namespace != namespace || e.InvolvedObject.Name != name {
			continue
		}
		if best == nil || eventTime(e).After(eventTime(best)) {
			best = e
		}
	}
	if best == nil {
		return ""
	}
	return strings.TrimSpace(best.Message)
}

func findLV(in *Input, key string) *topolvm.LogicalVolume {
	for i := range in.LVs {
		if in.LVs[i].Key() == key {
			return &in.LVs[i]
		}
	}
	return nil
}

// BuildHeadroom resolves claim → PV → LV → node and compares growth with free space there.
func BuildHeadroom(in Input, namespace, name string, size *int64) (Headroom, error) {
	pvc, err := findPVC(&in, namespace, name)
	if err != nil {
		return Headroom{}, err
	}
	ref := namespace + "/" + name
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		return Headroom{}, fmt.Errorf("persistentvolumeclaim %s is %s, not Bound", ref, pvc.Status.Phase)
	}
	var pv *corev1.PersistentVolume
	for i := range in.PVs {
		if in.PVs[i].Name == pvc.Spec.VolumeName {
			pv = &in.PVs[i]
		}
	}
	if pv == nil {
		return Headroom{}, fmt.Errorf("persistentvolume %s of %s not found", pvc.Spec.VolumeName, ref)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != in.Driver.Name {
		return Headroom{}, fmt.Errorf("persistentvolumeclaim %s is not a TopoLVM volume", ref)
	}
	lv := findLV(&in, pv.Spec.CSI.VolumeHandle)
	if lv == nil {
		return Headroom{}, fmt.Errorf("no LogicalVolume with volumeID %s for %s", pv.Spec.CSI.VolumeHandle, ref)
	}

	class := classResolver(&in)(lv)
	h := Headroom{
		Namespace: namespace, PVC: name, PV: pv.Name, LV: lv.Name, Node: lv.NodeName, DeviceClass: class,
		Type: classTypes(&in)(lv.NodeName, class), CurrentBytes: lv.UsedBytes(), RequestedBytes: size, Autoresizer: map[string]string{},
		LastResizeFailure: lastEvent(&in, "PersistentVolumeClaim", namespace, name, "VolumeResizeFailed"),
	}
	if pvc.Spec.StorageClassName != nil {
		h.StorageClass = *pvc.Spec.StorageClassName
		if sc, err := findStorageClass(&in, h.StorageClass); err == nil {
			h.AllowExpansion = sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion
			for k, v := range sc.Annotations {
				if strings.HasPrefix(k, autoresizerPrefix) {
					h.Autoresizer[k] = v
				}
			}
		}
	}
	for k, v := range pvc.Annotations {
		if strings.HasPrefix(k, autoresizerPrefix) {
			h.Autoresizer[k] = v
		}
	}
	for _, n := range in.Nodes {
		if free, ok := n.Capacity[class]; ok && n.Name == lv.NodeName {
			h.FreeBytes = new(free)
			if h.Type != topolvm.TypeThin {
				h.MaxBytes = new(h.CurrentBytes + free)
			}
		}
	}
	if size != nil && h.FreeBytes != nil {
		h.Fits = new(*size-h.CurrentBytes <= *h.FreeBytes)
	}
	return h, nil
}
