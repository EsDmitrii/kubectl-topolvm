package report_test

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

func TestBuildNode(t *testing.T) {
	nr, err := report.BuildNode(load(t, "node"), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if nr.Node != "node-a" || nr.State == nil || !nr.State.Ready || len(nr.Pools) != 1 || nr.Pools[0].Volumes != 3 {
		t.Errorf("header = %+v pools = %+v", nr, nr.Pools)
	}
	byPVC := map[string]report.NodeVolume{}
	for _, v := range nr.Volumes {
		byPVC[str(v.PVC)] = v
	}
	if v := byPVC["data-db-0"]; v.Owner != "StatefulSet/db" || v.SingleCopy == nil || !*v.SingleCopy || !reflect.DeepEqual(v.Pods, []string{"db-0"}) {
		t.Errorf("db = %+v", v)
	}
	if v := byPVC["data-ha-0"]; v.Owner != "StatefulSet/ha" || v.SingleCopy == nil || *v.SingleCopy {
		t.Errorf("ha = %+v", v)
	}
	if v := byPVC["old"]; v.Owner != "" || v.SingleCopy == nil || *v.SingleCopy || v.Status != "Released" {
		t.Errorf("old = %+v", v)
	}
	if !reflect.DeepEqual(nr.DeletedOnNodeDelete, []string{"ns-a/data-db-0", "ns-a/data-ha-0"}) {
		t.Errorf("deleted on node delete = %v", nr.DeletedOnNodeDelete)
	}
	want := []report.Placement{
		{Volume: "ns-a/data-ha-0", DeviceClass: "ssd", SizeBytes: 80 * gi, Target: ""},
		{Volume: "ns-a/data-db-0", DeviceClass: "ssd", SizeBytes: 50 * gi, Target: "node-b"},
		{Volume: "ns-a/old", DeviceClass: "ssd", SizeBytes: 10 * gi, Target: "node-b"},
	}
	if !reflect.DeepEqual(nr.Evacuation, want) {
		t.Errorf("evacuation = %+v", nr.Evacuation)
	}
}

func TestBuildNodeUnknown(t *testing.T) {
	if _, err := report.BuildNode(load(t, "node"), "x"); err == nil || err.Error() != `node "x" is not a TopoLVM node` {
		t.Errorf("err = %v", err)
	}
}

func TestEvacuationSkipsVolumesBeingDeleted(t *testing.T) {
	in := load(t, "node")
	nr, err := report.BuildNode(in, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	var lv string
	for _, v := range nr.Volumes {
		if str(v.PVC) == "data-db-0" {
			lv = v.LV
		}
	}
	deleted := in.Now.Add(-time.Hour)
	for i := range in.LVs {
		if in.LVs[i].Name == lv {
			in.LVs[i].DeletionTimestamp = &deleted
			in.LVs[i].Finalizers = []string{in.Driver.LVFinalizer()}
		}
	}
	if nr, err = report.BuildNode(in, "node-a"); err != nil {
		t.Fatal(err)
	}
	for _, p := range nr.Evacuation {
		if p.Volume == "ns-a/data-db-0" {
			t.Errorf("a volume being deleted needs no new node: %+v", nr.Evacuation)
		}
	}
	if len(nr.Evacuation) != 2 {
		t.Errorf("evacuation = %+v", nr.Evacuation)
	}
}

func TestNodeDeleteFollowsTheController(t *testing.T) {
	nr, err := report.BuildNode(load(t, "node"), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if nr.LogicalVolumesDeleted != 3 || !reflect.DeepEqual(nr.ClaimsLeftWithoutLV, []string{"ns-a/data-db-0", "ns-a/data-ha-0", "ns-a/old"}) {
		t.Errorf("Retain PVs stay without their LVs: lvs=%d left=%v", nr.LogicalVolumesDeleted, nr.ClaimsLeftWithoutLV)
	}

	in := load(t, "node")
	for i := range in.PVCs {
		if in.PVCs[i].Name == "data-db-0" {
			in.PVCs[i].Spec.StorageClassName = new("deleted-class")
		}
	}
	for i := range in.PVs {
		if in.PVs[i].Name == "pvc-ha0" {
			in.PVs[i].Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		}
	}
	if nr, err = report.BuildNode(in, "node-a"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nr.DeletedOnNodeDelete, []string{"ns-a/data-ha-0"}) {
		t.Errorf("the controller deletes only claims of its own StorageClasses: %v", nr.DeletedOnNodeDelete)
	}
	if !reflect.DeepEqual(nr.ClaimsLeftWithoutLV, []string{"ns-a/data-db-0", "ns-a/old"}) {
		t.Errorf("a deleted claim under Delete takes its PV along: %v", nr.ClaimsLeftWithoutLV)
	}

	in.StorageClasses = nil
	if nr, err = report.BuildNode(in, "node-a"); err != nil || nr.DeletedOnNodeDelete != nil || nr.ClaimsLeftWithoutLV != nil {
		t.Errorf("without StorageClasses both lists are unknown: %v %v %v", nr.DeletedOnNodeDelete, nr.ClaimsLeftWithoutLV, err)
	}
}

func TestNodeWithoutFinalizerDeletesNothing(t *testing.T) {
	in := load(t, "node")
	s := in.NodeStates["node-a"]
	s.HasFinalizer = false
	in.NodeStates["node-a"] = s
	nr, err := report.BuildNode(in, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(nr.DeletedOnNodeDelete) != 0 || nr.DeletedOnNodeDelete == nil || nr.LogicalVolumesDeleted != 0 || len(nr.ClaimsLeftWithoutLV) != 0 {
		t.Errorf("without the finalizer the controller deletes nothing: %v %d %v", nr.DeletedOnNodeDelete, nr.LogicalVolumesDeleted, nr.ClaimsLeftWithoutLV)
	}
}

func TestNodeDeleteUnknownWithoutClaims(t *testing.T) {
	in := load(t, "node")
	in.PVCs = nil
	nr, err := report.BuildNode(in, "node-a")
	if err != nil || nr.DeletedOnNodeDelete != nil || nr.ClaimsLeftWithoutLV != nil {
		t.Errorf("without the claims nothing is known about what goes: %v %v %v", nr.DeletedOnNodeDelete, nr.ClaimsLeftWithoutLV, err)
	}
}

func TestEvacuationUsesUpTargets(t *testing.T) {
	node := func(name string, free int64) topolvm.Node {
		return topolvm.Node{Name: name, Capacity: map[string]int64{"00default": free, "ssd": free}}
	}
	in := report.Input{
		Driver: topolvm.Current,
		Nodes:  []topolvm.Node{node("from", 100*gi), node("small", 15*gi), node("cordoned", 500*gi), node("big", 25*gi)},
		NodeStates: map[string]report.NodeState{
			"from": {Ready: true, HasFinalizer: true}, "small": {Ready: true, HasFinalizer: true},
			"cordoned": {Ready: true, Unschedulable: true, HasFinalizer: true}, "big": {Ready: true, HasFinalizer: true},
		},
		LVs: []topolvm.LogicalVolume{
			{Name: "a", UID: "a", VolumeID: "a", NodeName: "from", SizeBytes: 10 * gi},
			{Name: "b", UID: "b", VolumeID: "b", NodeName: "from", SizeBytes: 10 * gi},
			{Name: "c", UID: "c", VolumeID: "c", NodeName: "from", SizeBytes: 10 * gi},
			{Name: "d", UID: "d", VolumeID: "d", NodeName: "from", SizeBytes: 10 * gi},
		},
	}
	nr, err := report.BuildNode(in, "from")
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]int{}
	for _, p := range nr.Evacuation {
		targets[p.Target]++
	}
	if want := map[string]int{"small": 1, "big": 2, "": 1}; !reflect.DeepEqual(targets, want) {
		t.Errorf("each placement takes its room and a cordoned node takes none: %v", targets)
	}
}

func TestSingleCopyUnknownWithoutPods(t *testing.T) {
	in := load(t, "node")
	in.Pods = nil
	nr, err := report.BuildNode(in, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range nr.Volumes {
		if v.SingleCopy != nil {
			t.Errorf("without pods nothing tells a single copy from a replicated one: %+v", v)
		}
	}
}
