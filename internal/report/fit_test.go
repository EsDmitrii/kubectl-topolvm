package report_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

func fitNodes(rows []report.FitRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Node)
	}
	return out
}

func TestFit50Gi(t *testing.T) {
	res, err := report.BuildFit(load(t, "fit"), report.FitOptions{Size: 50 * gi})
	if err != nil {
		t.Fatal(err)
	}
	if got := fitNodes(res.Rows); !reflect.DeepEqual(got, []string{"node-a", "node-c", "node-b"}) {
		t.Fatalf("rows = %v", got)
	}
	a, c, b := res.Rows[0], res.Rows[1], res.Rows[2]
	if !a.Fits || a.Count != 2 || i64(a.FreeAfterBytes) != "53687091200" || a.DeviceClass != "ssd" || len(a.Notes) != 0 {
		t.Errorf("node-a = %+v", a)
	}
	if !c.Fits || c.Count != 1 || !reflect.DeepEqual(c.Notes, []string{"taint dedicated=db:NoSchedule"}) {
		t.Errorf("node-c = %+v", c)
	}
	if b.Fits || b.Count != 0 || b.FreeAfterBytes != nil || !reflect.DeepEqual(b.Notes, []string{"cordoned"}) {
		t.Errorf("node-b = %+v", b)
	}
	if res.NodesThatFit != 2 || res.Replicas != 1 || !res.OK {
		t.Errorf("result = %+v", res)
	}
}

func TestFitReplicas3(t *testing.T) {
	res, err := report.BuildFit(load(t, "fit"), report.FitOptions{Size: 50 * gi, Replicas: 3})
	if err != nil || res.OK || res.Replicas != 3 {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestFitStorageClassClass(t *testing.T) {
	res, err := report.BuildFit(load(t, "fit"), report.FitOptions{Size: 10 * gi, StorageClass: "topolvm-fast"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Node != "node-a" || res.Rows[0].DeviceClass != "dc2" || res.Rows[0].Count != 2 {
		t.Errorf("rows = %+v", res.Rows)
	}
	res, err = report.BuildFit(load(t, "fit"), report.FitOptions{Size: 10 * gi, DeviceClass: "dc2"})
	if err != nil || len(res.Rows) != 1 {
		t.Errorf("--device-class dc2: %+v %v", res.Rows, err)
	}
}

func TestFitForeignSC(t *testing.T) {
	_, err := report.BuildFit(load(t, "fit"), report.FitOptions{Size: gi, StorageClass: "local-path"})
	if err == nil || err.Error() != `storageclass "local-path" is not provisioned by topolvm.io` {
		t.Errorf("err = %v", err)
	}
	_, err = report.BuildFit(load(t, "fit"), report.FitOptions{Size: gi, StorageClass: "nope"})
	if err == nil || !strings.Contains(err.Error(), `storageclass "nope" not found`) {
		t.Errorf("err = %v", err)
	}
}

func TestHeadroom(t *testing.T) {
	h, err := report.BuildHeadroom(load(t, "fit"), "ns-a", "data", nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.PV != "pvc-data" || h.Node != "node-a" || h.DeviceClass != "ssd" || h.CurrentBytes != 10*gi ||
		i64(h.FreeBytes) != "107374182400" || i64(h.MaxBytes) != "118111600640" || !h.AllowExpansion ||
		h.Autoresizer["resize.topolvm.io/storage_limit"] != "200Gi" || h.Autoresizer["resize.topolvm.io/enabled"] != "true" ||
		h.LastResizeFailure != "not enough space" || h.Fits != nil {
		t.Errorf("headroom = %+v", h)
	}
	size := 200 * gi
	h, err = report.BuildHeadroom(load(t, "fit"), "ns-a", "data", &size)
	if err != nil || h.Fits == nil || *h.Fits {
		t.Errorf("200Gi: %+v %v", h, err)
	}
	size = 50 * gi
	if h, _ = report.BuildHeadroom(load(t, "fit"), "ns-a", "data", &size); h.Fits == nil || !*h.Fits {
		t.Errorf("50Gi must fit: %+v", h)
	}
}

func TestHeadroomErrors(t *testing.T) {
	if _, err := report.BuildHeadroom(load(t, "fit"), "ns-a", "nope", nil); err == nil || err.Error() != `persistentvolumeclaim ns-a/nope not found` {
		t.Errorf("err = %v", err)
	}
	in := load(t, "fit")
	in.PVCs = nil
	if _, err := report.BuildHeadroom(in, "ns-a", "data", nil); err == nil || !strings.Contains(err.Error(), "persistentvolumeclaims") {
		t.Errorf("err = %v", err)
	}
}

func TestFitNodeFilter(t *testing.T) {
	res, err := report.BuildFit(load(t, "fit"), report.FitOptions{Size: 50 * gi, Nodes: []string{"node-c"}})
	if err != nil || len(res.Rows) != 1 || res.Rows[0].Node != "node-c" {
		t.Errorf("rows = %+v err = %v", res.Rows, err)
	}
}

func TestFitCountsOnlySchedulableNodes(t *testing.T) {
	node := func(name string) topolvm.Node {
		return topolvm.Node{Name: name, Capacity: map[string]int64{"00default": 100 * gi, "ssd": 100 * gi}}
	}
	in := report.Input{
		Driver: topolvm.Current,
		Nodes:  []topolvm.Node{node("ready"), node("down"), node("cordoned")},
		NodeStates: map[string]report.NodeState{
			"ready": {Ready: true}, "down": {Ready: false}, "cordoned": {Ready: true, Unschedulable: true},
		},
	}
	res, err := report.BuildFit(in, report.FitOptions{Size: 10 * gi, Replicas: 1})
	if err != nil || res.NodesThatFit != 1 || !res.OK || len(res.Rows) != 3 {
		t.Errorf("NotReady and cordoned nodes are listed but do not count: %+v %v", res, err)
	}
	if res, _ = report.BuildFit(in, report.FitOptions{Size: 10 * gi, Replicas: 2}); res.OK {
		t.Errorf("two replicas need two schedulable nodes: %+v", res)
	}
	in.NodeStates["down"] = report.NodeState{Ready: true}
	if res, _ = report.BuildFit(in, report.FitOptions{Size: 10 * gi, Replicas: 2}); !res.OK || res.NodesThatFit != 2 {
		t.Errorf("exactly as many nodes as replicas fits: %+v", res)
	}
}
