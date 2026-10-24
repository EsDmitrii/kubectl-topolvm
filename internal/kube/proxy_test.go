package kube

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

const liveMetrics = `# HELP topolvm_volumegroup_available_bytes LVM VG available bytes under lvmd management
# TYPE topolvm_volumegroup_available_bytes gauge
topolvm_volumegroup_available_bytes{device_class="ssd",node="worker-b"} 1.0e+11
topolvm_volumegroup_size_bytes{device_class="ssd",node="worker-b"} 5.0e+11
topolvm_thinpool_data_percent{device_class="thin",node="worker-b"} 42.5
topolvm_thinpool_metadata_percent{device_class="thin",node="worker-b"} 3.25
topolvm_thinpool_size_bytes{device_class="thin",node="worker-b"} 1.073741824e+11
go_goroutines 42
`

func TestParseMetrics(t *testing.T) {
	m, err := ParseMetrics(strings.NewReader(liveMetrics))
	if err != nil {
		t.Fatal(err)
	}
	ssd := m["worker-b"]["ssd"]
	if ssd.VGSizeBytes == nil || *ssd.VGSizeBytes != 500000000000 || ssd.VGAvailableBytes == nil || *ssd.VGAvailableBytes != 100000000000 {
		t.Errorf("ssd = %+v", ssd)
	}
	thin := m["worker-b"]["thin"]
	if thin.ThinDataPercent == nil || *thin.ThinDataPercent != 42.5 || thin.ThinMetadataPercent == nil || *thin.ThinMetadataPercent != 3.25 ||
		thin.ThinPoolSizeBytes == nil || *thin.ThinPoolSizeBytes != 107374182400 {
		t.Errorf("thin = %+v", thin)
	}
	if _, err := ParseMetrics(strings.NewReader(`topolvm_volumegroup_size_bytes{device_class="ssd",node="n"} abc` + "\n")); err == nil {
		t.Error("want an error for a bad sample")
	}
}

const summary = `{"node": {"nodeName": "node-1"}, "pods": [
 {"podRef": {"name": "pg-1", "namespace": "ns-1"}, "volume": [
   {"name": "pgdata", "pvcRef": {"name": "pg-1", "namespace": "ns-1"}, "usedBytes": 1073741824, "capacityBytes": 10737418240,
    "availableBytes": 9663676416, "inodesUsed": 1200, "inodes": 655360},
   {"name": "kube-api-access", "usedBytes": 12288}]},
 {"podRef": {"name": "raw", "namespace": "ns-1"}, "volume": [
   {"name": "block", "pvcRef": {"name": "raw", "namespace": "ns-1"}, "capacityBytes": 5368709120}]}]}`

func TestParseSummary(t *testing.T) {
	u, err := ParseSummary(strings.NewReader(summary))
	if err != nil {
		t.Fatal(err)
	}
	if len(u) != 2 {
		t.Fatalf("usage = %+v", u)
	}
	pg := u["ns-1/pg-1"]
	if pg.UsedBytes == nil || *pg.UsedBytes != 1073741824 || *pg.CapacityBytes != 10737418240 || *pg.InodesUsed != 1200 || *pg.Inodes != 655360 {
		t.Errorf("pg = %+v", pg)
	}
	if raw := u["ns-1/raw"]; raw.UsedBytes != nil || raw.CapacityBytes == nil {
		t.Errorf("block volume has no used bytes: %+v", raw)
	}
}

func metricsPod(name, node string, port int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "topolvm-system"},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{
			{Name: "topolvm-node", Ports: []corev1.ContainerPort{{Name: "healthz", ContainerPort: 9808}, {Name: "metrics", ContainerPort: port}}}}},
	}
}

func TestCollectMetrics(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("worker-b")}, nil, metricsPod("topolvm-node-x", "worker-b", 8181))
	c := f.clients()
	var paths []string
	c.Raw = func(_ context.Context, path string) ([]byte, error) {
		paths = append(paths, path)
		return []byte(liveMetrics), nil
	}
	o := opts()
	o.Needs = NeedTopoLVMPods | NeedMetrics
	in, err := Collect(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/api/v1/namespaces/topolvm-system/pods/topolvm-node-x:8181/proxy/metrics" {
		t.Errorf("paths = %v", paths)
	}
	if m := in.Metrics["worker-b"]["ssd"]; m.VGSizeBytes == nil {
		t.Errorf("metrics = %+v", in.Metrics)
	}
}

func TestUsageForbiddenWarns(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a"), topoNode("b")}, []runtime.Object{lvObj("lv1", "a"), lvObj("lv2", "b")})
	c := f.clients()
	c.Raw = func(_ context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "/nodes/a/") {
			return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes/proxy"}, "a", errors.New("no"))
		}
		return []byte(summary), nil
	}
	o := opts()
	o.Needs = NeedUsage
	in, err := Collect(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cannot get nodes/proxy: forbidden; no usage from 1 of 2 nodes (kubectl topolvm rbac --with usage prints the rule)"}
	if len(in.Usage) != 2 || !reflect.DeepEqual(in.Warnings, want) {
		t.Errorf("usage = %v warnings = %q", in.Usage, in.Warnings)
	}
}

func TestMetricsForbiddenWarnsOnce(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a"), topoNode("b")}, nil,
		metricsPod("topolvm-node-a", "a", 8080), metricsPod("topolvm-node-b", "b", 8080))
	c := f.clients()
	c.Raw = func(_ context.Context, path string) ([]byte, error) {
		return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods/proxy"}, "x", errors.New("no"))
	}
	o := opts()
	o.Needs = NeedTopoLVMPods | NeedMetrics
	in, err := Collect(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cannot get pods/proxy in topolvm-system: forbidden; no metrics from 2 of 2 topolvm-node pods " +
		"(kubectl topolvm rbac --with metrics prints the rule)"}
	if !reflect.DeepEqual(in.Warnings, want) {
		t.Errorf("warnings = %q", in.Warnings)
	}
}

func TestUsageDoesNotWaitForASilentKubelet(t *testing.T) {
	prev := proxyTimeout
	proxyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { proxyTimeout = prev })
	f := newFakes([]runtime.Object{topoNode("a"), topoNode("b")}, []runtime.Object{lvObj("lv1", "a"), lvObj("lv2", "b")})
	c := f.clients()
	c.Raw = func(ctx context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "/nodes/a/") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []byte(summary), nil
	}
	o := opts()
	o.Needs = NeedUsage
	done := make(chan report.Input, 1)
	go func() {
		in, _ := Collect(context.Background(), c, o)
		done <- in
	}()
	select {
	case in := <-done:
		want := []string{"usage from a unavailable: no answer in 50ms"}
		if !reflect.DeepEqual(in.Warnings, want) {
			t.Errorf("warnings = %q", in.Warnings)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("one silent kubelet must not hang --usage")
	}
}
