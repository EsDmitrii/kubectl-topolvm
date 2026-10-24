package report_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

func str(p *string) string {
	if p == nil {
		return "nil"
	}
	return *p
}

func testPV(name, handle string, phase corev1.PersistentVolumePhase) corev1.PersistentVolume {
	return corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "topolvm.io", VolumeHandle: handle}},
			ClaimRef:                      &corev1.ObjectReference{Namespace: "ns", Name: "claim-" + name},
			StorageClassName:              "topolvm-retain",
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		},
		Status: corev1.PersistentVolumeStatus{Phase: phase},
	}
}

func oneNode() []topolvm.Node {
	return []topolvm.Node{{Name: "n", Capacity: map[string]int64{"00default": 10 * gi, "ssd": 10 * gi}}}
}

func TestBuildVolumesEmptyClassResolved(t *testing.T) {
	r := build(load(t, "single-thick"))
	if len(r.Volumes) != 2 {
		t.Fatalf("volumes = %+v", r.Volumes)
	}
	for _, v := range r.Volumes {
		if v.DeviceClass != "ssd" || v.Status != "Bound" || v.Kind != "volume" {
			t.Errorf("volume = %+v", v)
		}
	}
	v := r.Volumes[0]
	if str(v.Namespace) != "ns-a" || str(v.PVC) != "data-1" || str(v.StorageClass) != "topolvm-retain" ||
		str(v.ReclaimPolicy) != "Retain" || str(v.PV) != "pvc-1111" || v.SizeBytes != 10*gi {
		t.Errorf("first volume = %+v", v)
	}
}

func TestBuildVolumesJoinByVolumeHandle(t *testing.T) {
	in := report.Input{
		Driver: topolvm.Current,
		Nodes:  oneNode(),
		LVs:    []topolvm.LogicalVolume{{Name: "pvc-x", UID: "u-x", VolumeID: "u-x", NodeName: "n", SizeBytes: gi}},
		PVs:    []corev1.PersistentVolume{testPV("pv-static", "u-x", corev1.VolumeBound)},
	}
	v := report.Build(in, report.Options{}).Volumes[0]
	if str(v.PV) != "pv-static" || v.Status != "Bound" {
		t.Errorf("volume = %+v", v)
	}
}

func TestBuildVolumesSnapshotAndClone(t *testing.T) {
	r := build(load(t, "multi-class"))
	byLV := map[string]report.Volume{}
	for _, v := range r.Volumes {
		byLV[v.LV] = v
	}
	if s := byLV["snapshot-1"]; s.Kind != "snapshot" || s.Status != "Snapshot" || s.PV != nil || str(s.Source) != "pvc-src" {
		t.Errorf("snapshot = %+v", s)
	}
	if c := byLV["pvc-clone"]; c.Kind != "clone" || c.Status != "Bound" {
		t.Errorf("clone = %+v", c)
	}
	if d := byLV["pvc-d1"]; d.DeviceClass != "dc1" {
		t.Errorf("empty class must resolve to dc1: %+v", d)
	}
}

func TestBuildVolumesStatuses(t *testing.T) {
	small := 5 * gi
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	base := func() topolvm.LogicalVolume {
		return topolvm.LogicalVolume{Name: "lv", UID: "u", VolumeID: "u", NodeName: "n", SizeBytes: 10 * gi}
	}
	cases := []struct {
		name   string
		mutate func(*topolvm.LogicalVolume)
		pvs    []corev1.PersistentVolume
		want   string
	}{
		{"error", func(lv *topolvm.LogicalVolume) { lv.Code = 13 }, []corev1.PersistentVolume{testPV("p", "u", corev1.VolumeBound)}, "Error"},
		{"deleting", func(lv *topolvm.LogicalVolume) { lv.DeletionTimestamp = &now }, []corev1.PersistentVolume{testPV("p", "u", corev1.VolumeBound)}, "Deleting"},
		{"resizing", func(lv *topolvm.LogicalVolume) { lv.CurrentSizeBytes = &small }, []corev1.PersistentVolume{testPV("p", "u", corev1.VolumeBound)}, "Resizing"},
		{"released", func(*topolvm.LogicalVolume) {}, []corev1.PersistentVolume{testPV("p", "u", corev1.VolumeReleased)}, "Released"},
		{"orphan", func(*topolvm.LogicalVolume) {}, []corev1.PersistentVolume{}, "Orphan"},
		{"unknown", func(*topolvm.LogicalVolume) {}, nil, "<unknown>"},
		{"same name, other handle", func(lv *topolvm.LogicalVolume) { lv.VolumeID = "" }, []corev1.PersistentVolume{testPV("lv", "other", corev1.VolumeBound)}, "Orphan"},
	}
	for _, c := range cases {
		lv := base()
		c.mutate(&lv)
		in := report.Input{Driver: topolvm.Current, Nodes: oneNode(), LVs: []topolvm.LogicalVolume{lv}, PVs: c.pvs}
		if got := report.Build(in, report.Options{}).Volumes[0].Status; got != c.want {
			t.Errorf("%s: status = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBuildVolumesNamespaceFilter(t *testing.T) {
	r := report.Build(load(t, "single-thick"), report.Options{Namespace: "ns-a"})
	if len(r.Volumes) != 1 || str(r.Volumes[0].PVC) != "data-1" {
		t.Errorf("volumes = %+v", r.Volumes)
	}
	r = report.Build(load(t, "multi-class"), report.Options{Namespace: "ns-b"})
	for _, v := range r.Volumes {
		if v.Kind == "snapshot" {
			t.Errorf("a snapshot without a claim must not pass a namespace filter: %+v", v)
		}
	}
}

func TestBuildVolumesNodeAndClassFilter(t *testing.T) {
	r := report.Build(load(t, "multi-class"), report.Options{DeviceClasses: []string{"dc1"}})
	if len(r.Volumes) != 1 || r.Volumes[0].LV != "pvc-d1" {
		t.Errorf("volumes = %+v", r.Volumes)
	}
}
