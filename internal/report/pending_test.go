package report_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

func TestPendingReasons(t *testing.T) {
	rows := report.BuildPending(load(t, "pending"), report.Options{})
	got := map[string]report.PendingPVC{}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		got[r.PVC] = r
		names = append(names, r.PVC)
	}
	if !reflect.DeepEqual(names, []string{"big", "imm", "noconsumer", "prov"}) {
		t.Fatalf("rows = %v", names)
	}
	if r := got["imm"]; r.Reason != "Immediate" || r.Detail != "StorageClass topolvm-immediate uses Immediate binding; TopoLVM recommends WaitForFirstConsumer" {
		t.Errorf("imm = %+v", r)
	}
	if r := got["noconsumer"]; r.Reason != "NoConsumer" || r.Detail != "waiting for a pod that uses it" {
		t.Errorf("noconsumer = %+v", r)
	}
	if r := got["big"]; r.Reason != "Unschedulable" || r.Detail != "no node has 100Gi free in class ssd (max 50Gi on node-a)" ||
		!strings.Contains(r.LastEvent, "did not have enough free storage") || r.RequestBytes != 100*gi || r.DeviceClass != "ssd" {
		t.Errorf("big = %+v", r)
	}
	if r := got["prov"]; r.Reason != "Provisioning" || r.Detail != "node-b has only 20Gi free in class ssd" ||
		r.SelectedNode != "node-b" || !strings.Contains(r.LastEvent, "ResourceExhausted") {
		t.Errorf("prov = %+v", r)
	}
	if rows := report.BuildPending(load(t, "pending"), report.Options{Namespace: "other"}); len(rows) != 0 {
		t.Errorf("namespace filter: %+v", rows)
	}
}

func TestChecksConsistency(t *testing.T) {
	r := build(load(t, "consistency"))
	want := []string{
		"immediate-binding storageclass/topolvm-immediate",
		"capacity-missing node/node-b",
		"capacity-mismatch node/node-a",
		"stale-class-annotation node/node-a",
		"node-without-capacity node/node-c",
		"topolvm-node-not-ready node/node-b",
		"pvc-pending persistentvolumeclaim/ns-a/big",
		"pvc-resize-stuck persistentvolumeclaim/ns-a/grow",
	}
	if got := pairs(r.Findings); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	byKey := map[string]report.Finding{}
	for _, f := range r.Findings {
		byKey[f.Check] = f
	}
	details := map[string]string{
		"immediate-binding":      "volumeBindingMode Immediate; TopoLVM recommends WaitForFirstConsumer",
		"capacity-missing":       "no CSIStorageCapacity for storageclass topolvm-wffc: the scheduler will skip this node",
		"capacity-mismatch":      "CSIStorageCapacity csisc-a says 60Gi, node annotation says 50Gi: the scheduler may pick this node for volumes that do not fit",
		"stale-class-annotation": "class old is not in the lvmd config; the annotation may be frozen",
		"node-without-capacity":  "registered with topolvm.io but has no capacity annotations",
		"topolvm-node-not-ready": "no ready topolvm-node pod",
		"pvc-pending":            "Unschedulable: no node has 100Gi free in class ssd (max 50Gi on node-a)",
		"pvc-resize-stuck":       "requested 20Gi, capacity 10Gi; last VolumeResizeFailed: rpc error: code = Internal desc = not enough space",
	}
	for id, d := range details {
		if byKey[id].Detail != d {
			t.Errorf("%s detail = %q, want %q", id, byKey[id].Detail, d)
		}
	}
	if f := byKey["pvc-resize-stuck"]; i64(f.AgeSeconds) != "3600" || f.Node != "node-a" {
		t.Errorf("pvc-resize-stuck = %+v", f)
	}
}

func TestStorageCapacityDisabled(t *testing.T) {
	in := load(t, "consistency")
	in.CSIDriver = &storagev1.CSIDriver{}
	in.CSIDriver.Name = "topolvm.io"
	r := build(in)
	if got := pairs(r.Findings); len(got) == 0 || got[0] != "storage-capacity-disabled csidriver/topolvm.io" {
		t.Errorf("findings = %v", got)
	}
	for _, f := range r.Findings {
		if f.Check == "capacity-missing" || f.Check == "capacity-mismatch" {
			t.Errorf("capacity checks must not run without storage capacity tracking: %+v", f)
		}
	}
}

func TestCapacityMismatchGrace(t *testing.T) {
	in := load(t, "consistency")
	in.LVs = append(in.LVs, topolvm.LogicalVolume{Name: "fresh", UID: "f", VolumeID: "f", NodeName: "node-a", SizeBytes: gi,
		Created: in.Now.Add(-time.Minute)})
	for _, f := range build(in).Findings {
		if f.Check == "capacity-mismatch" {
			t.Errorf("a fresh LV on the node must hold the mismatch back: %+v", f)
		}
	}
}

func TestExtendedChecksSkipSilentlyWithoutData(t *testing.T) {
	r := build(load(t, "single-thick"))
	if len(r.Findings) != 0 || len(r.Warnings) != 0 {
		t.Errorf("findings %v warnings %q", pairs(r.Findings), r.Warnings)
	}
}

func TestCapacityMismatchAfterDeletion(t *testing.T) {
	in := load(t, "consistency")
	under := resource.MustParse("40Gi")
	in.Capacities[0].Capacity = &under
	for _, f := range build(in).Findings {
		if f.Check == "capacity-mismatch" {
			t.Errorf("a CSIStorageCapacity below the annotation is the normal state right after a deletion: %+v", f)
		}
	}
}

func TestCapacityMissingGrace(t *testing.T) {
	in := load(t, "consistency")
	for i := range in.CSINodes {
		if in.CSINodes[i].Name == "node-b" {
			in.CSINodes[i].CreationTimestamp = metav1.NewTime(in.Now.Add(-30 * time.Second))
		}
	}
	for _, f := range build(in).Findings {
		if f.Check == "capacity-missing" {
			t.Errorf("a node registered 30s ago has not been polled yet: %+v", f)
		}
	}
	in = load(t, "consistency")
	for i := range in.StorageClasses {
		in.StorageClasses[i].CreationTimestamp = metav1.NewTime(in.Now.Add(-time.Minute))
	}
	for _, f := range build(in).Findings {
		if f.Check == "capacity-missing" {
			t.Errorf("a StorageClass created a minute ago has not been polled yet: %+v", f)
		}
	}
}

func TestPendingFilters(t *testing.T) {
	rows := report.BuildPending(load(t, "pending"), report.Options{Nodes: []string{"node-b"}})
	if len(rows) != 1 || rows[0].PVC != "prov" {
		t.Errorf("--node node-b: %+v", rows)
	}
	if rows := report.BuildPending(load(t, "pending"), report.Options{DeviceClasses: []string{"nvme"}}); len(rows) != 0 {
		t.Errorf("--device-class nvme: %+v", rows)
	}
}

func TestPvcPendingAgesFromThePod(t *testing.T) {
	in := load(t, "consistency")
	for i := range in.Pods {
		if in.Pods[i].Name == "app-big" {
			in.Pods[i].CreationTimestamp = metav1.NewTime(in.Now.Add(-5 * time.Minute))
		}
	}
	for _, f := range build(in).Findings {
		if f.Check == "pvc-pending" {
			t.Errorf("the consumer pod is five minutes old; the claim has waited only that long: %+v", f)
		}
	}
}

func TestCapacityMismatchTakesTheLargestDuplicate(t *testing.T) {
	in := load(t, "consistency")
	exact := in.Capacities[0].DeepCopy()
	exact.Name = "csisc-a-fresh"
	exact.Capacity = new(resource.MustParse("50Gi"))
	in.Capacities = append(in.Capacities, *exact)
	found := false
	for _, f := range build(in).Findings {
		found = found || (f.Check == "capacity-mismatch" && strings.Contains(f.Detail, "csisc-a says 60Gi"))
	}
	if !found {
		t.Error("the scheduler takes any CSIStorageCapacity with room, so the overstated duplicate counts")
	}
}
