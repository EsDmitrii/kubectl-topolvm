package topolvm

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// LogicalVolume is the part of a TopoLVM LogicalVolume the plugin reads.
type LogicalVolume struct {
	Name, UID, NodeName, DeviceClass, Source, AccessType, VolumeID, Message string

	SizeBytes         int64
	CurrentSizeBytes  *int64
	Code              int64
	Created           time.Time
	DeletionTimestamp *time.Time
	Finalizers        []string
	Annotations       map[string]string
}

// Key is what PersistentVolumes reference in spec.csi.volumeHandle: status.volumeID, or the uid
// while the volume is still being created.
func (lv LogicalVolume) Key() string {
	if lv.VolumeID != "" {
		return lv.VolumeID
	}
	return lv.UID
}

// UsedBytes is the allocated size: status.currentSize when known, spec.size otherwise.
func (lv LogicalVolume) UsedBytes() int64 {
	if lv.CurrentSizeBytes != nil {
		return *lv.CurrentSizeBytes
	}
	return lv.SizeBytes
}

// OnDisk reports whether the LV takes space in its volume group. TopoLVM sets status.volumeID only
// once lvmd created the LV, and removes the disk LV before it drops its finalizer, so a deleted LV
// without that finalizer only waits for foreign ones.
func (lv LogicalVolume) OnDisk(finalizer string) bool {
	return lv.VolumeID != "" && (lv.DeletionTimestamp == nil || slices.Contains(lv.Finalizers, finalizer))
}

// IsSnapshot reports a read-only LV cut from another one; it never has a PersistentVolume.
func (lv LogicalVolume) IsSnapshot() bool { return lv.Source != "" && lv.AccessType == "ro" }

// LogicalVolumeFrom converts an unstructured LogicalVolume of either API group. On error the
// returned LV still carries its name and volume ID.
func LogicalVolumeFrom(u *unstructured.Unstructured) (LogicalVolume, error) {
	lv := LogicalVolume{
		Name:        u.GetName(),
		UID:         string(u.GetUID()),
		Created:     u.GetCreationTimestamp().UTC(),
		Finalizers:  u.GetFinalizers(),
		Annotations: u.GetAnnotations(),
	}
	if ts := u.GetDeletionTimestamp(); ts != nil {
		t := ts.UTC()
		lv.DeletionTimestamp = &t
	}
	lv.NodeName, _, _ = unstructured.NestedString(u.Object, "spec", "nodeName")
	lv.DeviceClass, _, _ = unstructured.NestedString(u.Object, "spec", "deviceClass")
	lv.Source, _, _ = unstructured.NestedString(u.Object, "spec", "source")
	lv.AccessType, _, _ = unstructured.NestedString(u.Object, "spec", "accessType")
	lv.VolumeID, _, _ = unstructured.NestedString(u.Object, "status", "volumeID")
	lv.Message, _, _ = unstructured.NestedString(u.Object, "status", "message")

	size, err := quantity(u.Object, "spec", "size")
	if err != nil {
		return lv, fmt.Errorf("logicalvolume %s: %w", lv.Name, err)
	}
	if size == nil {
		return lv, fmt.Errorf("logicalvolume %s: spec.size is missing", lv.Name)
	}
	lv.SizeBytes = *size
	if lv.CurrentSizeBytes, err = quantity(u.Object, "status", "currentSize"); err != nil {
		return lv, fmt.Errorf("logicalvolume %s: %w", lv.Name, err)
	}

	if raw, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "code"); found {
		switch v := raw.(type) {
		case int64:
			lv.Code = v
		case float64:
			lv.Code = int64(v)
		case json.Number:
			n, err := v.Int64()
			if err != nil {
				return lv, fmt.Errorf("logicalvolume %s: status.code: %w", lv.Name, err)
			}
			lv.Code = n
		case string:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return lv, fmt.Errorf("logicalvolume %s: status.code %q is not a number", lv.Name, v)
			}
			lv.Code = n
		}
	}
	return lv, nil
}

func quantity(obj map[string]any, fields ...string) (*int64, error) {
	raw, found, _ := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found || raw == nil {
		return nil, nil
	}
	var s string
	switch v := raw.(type) {
	case string:
		s = v
	case int64:
		s = fmt.Sprint(v)
	case float64:
		s = fmt.Sprint(int64(v))
	default:
		return nil, fmt.Errorf("%s: unexpected type %T", strings.Join(fields, "."), raw)
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", strings.Join(fields, "."), s, err)
	}
	n := q.Value()
	return &n, nil
}
