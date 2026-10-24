package topolvm

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func lvFromJSON(t *testing.T, s string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(s), &u.Object); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLogicalVolumeFromLiveShape(t *testing.T) {
	u := lvFromJSON(t, `{
		"apiVersion": "topolvm.io/v1", "kind": "LogicalVolume",
		"metadata": {"name": "pvc-6b1e0c7a-3f2d-4e8b-9a51-2c4d7e8f9a10",
			"uid": "0d9c8b7a-6f5e-4d3c-8b2a-1f0e9d8c7b6a",
			"finalizers": ["topolvm.io/logicalvolume"],
			"creationTimestamp": "2026-10-23T16:05:57Z"},
		"spec": {"name": "pvc-6b1e0c7a-3f2d-4e8b-9a51-2c4d7e8f9a10", "nodeName": "worker-a", "size": "10Gi"},
		"status": {"currentSize": "10Gi", "volumeID": "0d9c8b7a-6f5e-4d3c-8b2a-1f0e9d8c7b6a"}}`)
	lv, err := LogicalVolumeFrom(u)
	if err != nil {
		t.Fatal(err)
	}
	if lv.SizeBytes != 10737418240 {
		t.Errorf("SizeBytes = %d", lv.SizeBytes)
	}
	if lv.CurrentSizeBytes == nil || *lv.CurrentSizeBytes != 10737418240 {
		t.Errorf("CurrentSizeBytes = %v", lv.CurrentSizeBytes)
	}
	if lv.DeviceClass != "" || lv.Code != 0 {
		t.Errorf("DeviceClass = %q, Code = %d", lv.DeviceClass, lv.Code)
	}
	if lv.NodeName != "worker-a" || lv.VolumeID != lv.UID || lv.UID != "0d9c8b7a-6f5e-4d3c-8b2a-1f0e9d8c7b6a" {
		t.Errorf("identity fields wrong: %+v", lv)
	}
	if lv.Created.Format("2006-01-02T15:04:05Z") != "2026-10-23T16:05:57Z" {
		t.Errorf("Created = %v", lv.Created)
	}
	if len(lv.Finalizers) != 1 || lv.Finalizers[0] != "topolvm.io/logicalvolume" {
		t.Errorf("Finalizers = %v", lv.Finalizers)
	}
}

func TestLogicalVolumeFromQuantities(t *testing.T) {
	for size, want := range map[string]int64{"10G": 10000000000, "10737418240": 10737418240, "1.5Gi": 1610612736} {
		u := lvFromJSON(t, `{"metadata": {"name": "x", "uid": "u"}, "spec": {"nodeName": "n", "size": "`+size+`"}}`)
		lv, err := LogicalVolumeFrom(u)
		if err != nil {
			t.Fatalf("%s: %v", size, err)
		}
		if lv.SizeBytes != want {
			t.Errorf("%s: SizeBytes = %d, want %d", size, lv.SizeBytes, want)
		}
	}
	u := lvFromJSON(t, `{"metadata": {"name": "x"}, "spec": {"size": "ten"}}`)
	if _, err := LogicalVolumeFrom(u); err == nil || !strings.Contains(err.Error(), "spec.size") {
		t.Errorf("want spec.size error, got %v", err)
	}
}

func TestLogicalVolumeFromErrorAndSnapshot(t *testing.T) {
	u := lvFromJSON(t, `{"metadata": {"name": "snap", "uid": "u",
			"deletionTimestamp": "2026-10-24T10:00:00Z",
			"annotations": {"topolvm.io/pendingdeletion": "true"}},
		"spec": {"nodeName": "n", "size": "1Gi", "source": "pvc-a", "accessType": "ro", "deviceClass": "thin"},
		"status": {"code": 8, "message": "not enough space"}}`)
	lv, err := LogicalVolumeFrom(u)
	if err != nil {
		t.Fatal(err)
	}
	if lv.Code != 8 || lv.Message != "not enough space" {
		t.Errorf("Code = %d, Message = %q", lv.Code, lv.Message)
	}
	if lv.CurrentSizeBytes != nil {
		t.Errorf("CurrentSizeBytes = %v, want nil", *lv.CurrentSizeBytes)
	}
	if lv.Source != "pvc-a" || lv.AccessType != "ro" || lv.DeviceClass != "thin" {
		t.Errorf("spec fields wrong: %+v", lv)
	}
	if lv.DeletionTimestamp == nil || lv.Annotations["topolvm.io/pendingdeletion"] != "true" {
		t.Errorf("metadata fields wrong: %+v", lv)
	}
}

func TestNodeFrom(t *testing.T) {
	ann := map[string]string{
		"capacity.topolvm.io/00default": "121915834368",
		"capacity.topolvm.io/ssd":       "121915834368",
		"capacity.topolvm.io/broken":    "abc",
		"node.alpha.kubernetes.io/ttl":  "0",
	}
	n := NodeFrom("node-a", ann, Current)
	if len(n.Capacity) != 2 || n.Capacity["ssd"] != 121915834368 || n.Capacity[DefaultAlias] != 121915834368 {
		t.Errorf("Capacity = %v", n.Capacity)
	}
	if len(n.BadAnnotations) != 1 || n.BadAnnotations[0] != "capacity.topolvm.io/broken" {
		t.Errorf("BadAnnotations = %v", n.BadAnnotations)
	}
	if l := NodeFrom("node-a", ann, Legacy); len(l.Capacity) != 0 {
		t.Errorf("legacy Capacity = %v", l.Capacity)
	}
}

func TestDriverNames(t *testing.T) {
	if got := Legacy.CapacityPrefix(); got != "capacity.topolvm.cybozu.com/" {
		t.Errorf("CapacityPrefix = %q", got)
	}
	if got := Legacy.GVR(); got.Group != "topolvm.cybozu.com" || got.Version != "v1" || got.Resource != "logicalvolumes" {
		t.Errorf("GVR = %v", got)
	}
	if got := Current.TopologyKey(); got != "topology.topolvm.io/node" {
		t.Errorf("TopologyKey = %q", got)
	}
	if Current.PendingDeletionKey() != "topolvm.io/pendingdeletion" || Current.LVFinalizer() != "topolvm.io/logicalvolume" ||
		Current.ResizeRequestedAtKey() != "topolvm.io/resize-requested-at" || Current.NodeFinalizer() != "topolvm.io/node" {
		t.Errorf("annotation keys wrong")
	}
	if d, err := ParseDriver("topolvm.cybozu.com"); err != nil || d != Legacy {
		t.Errorf("ParseDriver legacy = %v, %v", d, err)
	}
	if _, err := ParseDriver("csi.example.com"); err == nil {
		t.Error("ParseDriver accepted a foreign driver")
	}
}

func TestParseLVMDConfig(t *testing.T) {
	live := "socket-name: /run/topolvm/lvmd.sock\ndevice-classes: \n  - default: true\n    name: ssd\n    spare-gb: 10\n    volume-group: vg0\n"
	got, err := ParseLVMDConfig(live)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "ssd" || got[0].VolumeGroup != "vg0" || got[0].Type != TypeThick ||
		!got[0].Default || got[0].SpareBytes == nil || *got[0].SpareBytes != 10<<30 {
		t.Errorf("live config = %+v", got)
	}

	mixed := `device-classes:
  - name: nvme-thin
    volume-group: vg1
    type: thin
    thin-pool: {name: pool0, overprovision-ratio: 10}
  - name: hdd
    volume-group: vg2
    default: true
`
	got, err = ParseLVMDConfig(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Type != TypeThin || got[0].OverprovisionRatio != 10 || got[0].SpareBytes != nil || got[0].ThinPool != "pool0" {
		t.Errorf("thin class = %+v", got[0])
	}
	if got[1].Type != TypeThick || !got[1].Default || got[1].SpareBytes == nil || *got[1].SpareBytes != 10<<30 {
		t.Errorf("hdd class = %+v", got[1])
	}
}

func cm(name string, data map[string]string) corev1.ConfigMap {
	return corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: data}
}

func TestClassesFromConfigMaps(t *testing.T) {
	thick := "device-classes:\n  - name: ssd\n    volume-group: vg0\n    default: true\n"
	thin := "device-classes:\n  - name: ssd\n    volume-group: vg0\n    type: thin\n    thin-pool: {name: p, overprovision-ratio: 2}\n"
	classes, warnings := ClassesFromConfigMaps([]corev1.ConfigMap{
		cm("topolvm-lvmd-0", map[string]string{"lvmd.yaml": thick}),
		cm("topolvm-lvmd-1", map[string]string{"lvmd.yaml": thin}),
		cm("unrelated", map[string]string{"other": "x"}),
	})
	if classes["ssd"].Type != TypeUnknown || len(warnings) != 1 {
		t.Errorf("conflict: classes = %+v, warnings = %v", classes, warnings)
	}

	classes, warnings = ClassesFromConfigMaps([]corev1.ConfigMap{
		cm("broken", map[string]string{"lvmd.yaml": "device-classes: [: :"}),
		cm("ok", map[string]string{"lvmd.yaml": thick}),
	})
	if len(warnings) != 1 || classes["ssd"].Type != TypeThick {
		t.Errorf("broken yaml: classes = %+v, warnings = %v", classes, warnings)
	}

	classes, warnings = ClassesFromConfigMaps([]corev1.ConfigMap{cm("unrelated", map[string]string{"other": "x"})})
	if classes != nil || len(warnings) != 0 {
		t.Errorf("no lvmd config: classes = %v, warnings = %v", classes, warnings)
	}
}

func TestClassesFromLVMVolumeGroups(t *testing.T) {
	thin := lvFromJSON(t, `{"apiVersion": "lvm.topolvm.io/v1alpha1", "kind": "LVMVolumeGroup",
		"metadata": {"name": "vg1", "namespace": "openshift-storage"},
		"spec": {"default": true, "thinPoolConfig": {"name": "thin-pool-1", "sizePercent": 90, "overprovisionRatio": 10}}}`)
	thick := lvFromJSON(t, `{"apiVersion": "lvm.topolvm.io/v1alpha1", "kind": "LVMVolumeGroup",
		"metadata": {"name": "vg2", "namespace": "openshift-storage"}, "spec": {}}`)
	got := ClassesFromLVMVolumeGroups([]unstructured.Unstructured{*thin, *thick})
	if c := got["vg1"]; c.Type != TypeThin || !c.Default || c.OverprovisionRatio != 10 || c.VolumeGroup != "vg1" || c.ThinPool != "thin-pool-1" {
		t.Errorf("vg1 = %+v", c)
	}
	if c := got["vg2"]; c.Type != TypeThick || c.Default || c.SpareBytes == nil || *c.SpareBytes != 0 {
		t.Errorf("vg2 = %+v", c)
	}
	single := ClassesFromLVMVolumeGroups([]unstructured.Unstructured{*thick})
	if !single["vg2"].Default {
		t.Errorf("a single LVMVolumeGroup is the default: %+v", single)
	}
	if ClassesFromLVMVolumeGroups(nil) != nil {
		t.Error("no objects must give nil")
	}
}

func TestClassesFromConfigMapsPerNodeGroupVGs(t *testing.T) {
	a := "device-classes:\n  - name: ssd\n    volume-group: vg-a\n    default: true\n    spare-gb: 1\n"
	b := "device-classes:\n  - name: ssd\n    volume-group: vg-b\n    default: true\n    spare-gb: 2\n"
	classes, warnings := ClassesFromConfigMaps([]corev1.ConfigMap{
		cm("topolvm-lvmd-0", map[string]string{"lvmd.yaml": a}),
		cm("topolvm-lvmd-1", map[string]string{"lvmd.yaml": b}),
	})
	ssd := classes["ssd"]
	if ssd.Type != TypeThick || !ssd.Default || ssd.VolumeGroup != "" || ssd.SpareBytes != nil || len(warnings) != 0 {
		t.Errorf("different VGs per node group are legitimate: %+v warnings %q", ssd, warnings)
	}
}

func lvmdPod(node, configMap string, containers ...string) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p-" + node}, Spec: corev1.PodSpec{NodeName: node}}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	p.Spec.Volumes = []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap}}}}}
	return p
}

func TestNodeClasses(t *testing.T) {
	a := "device-classes:\n  - name: ssd\n    volume-group: vg-a\n    default: true\n"
	b := "device-classes:\n  - name: ssd\n    volume-group: vg-b\n    default: true\n  - name: hdd\n    volume-group: vg-h\n"
	cms := []corev1.ConfigMap{cm("topolvm-lvmd-0", map[string]string{"lvmd.yaml": a}), cm("topolvm-lvmd-1", map[string]string{"lvmd.yaml": b})}
	pods := []corev1.Pod{
		lvmdPod("w1", "topolvm-lvmd-0", "lvmd"),
		lvmdPod("w2", "topolvm-lvmd-1", "lvmd"),
		lvmdPod("w3", "topolvm-lvmd-1", "topolvm-node"),
		lvmdPod("w4", "something-else", "lvmd"),
	}
	got := NodeClasses(pods, cms)
	if got["w1"]["ssd"].VolumeGroup != "vg-a" || got["w2"]["ssd"].VolumeGroup != "vg-b" || got["w2"]["hdd"].VolumeGroup != "vg-h" {
		t.Errorf("node classes = %+v", got)
	}
	if got["w3"]["hdd"].VolumeGroup != "vg-h" {
		t.Errorf("an embedded topolvm-node pod mounts the lvmd config too: %+v", got["w3"])
	}
	if _, ok := got["w4"]; ok {
		t.Errorf("a pod without an lvmd ConfigMap gives no classes: %+v", got["w4"])
	}
}

func TestLogicalVolumeStatusCodeAsString(t *testing.T) {
	u := lvFromJSON(t, `{"apiVersion": "topolvm.io/v1", "kind": "LogicalVolume",
		"metadata": {"name": "lv", "uid": "u"}, "spec": {"nodeName": "n", "size": "1Gi"},
		"status": {"volumeID": "u", "code": "13"}}`)
	if lv, err := LogicalVolumeFrom(u); err != nil || lv.Code != 13 {
		t.Errorf("code = %d, err = %v", lv.Code, err)
	}
	u = lvFromJSON(t, `{"apiVersion": "topolvm.io/v1", "kind": "LogicalVolume",
		"metadata": {"name": "lv", "uid": "u"}, "spec": {"nodeName": "n", "size": "1Gi"},
		"status": {"volumeID": "v", "code": "internal"}}`)
	if lv, err := LogicalVolumeFrom(u); err == nil || lv.Key() != "v" {
		t.Errorf("an unreadable code is an error that still names the volume: key %q, err %v", lv.Key(), err)
	}
}
