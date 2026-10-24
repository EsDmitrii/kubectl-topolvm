package kube

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func nodeMeta(name string, labels, annotations map[string]string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
	}
}

func topoNode(name string) *metav1.PartialObjectMetadata {
	return nodeMeta(name, map[string]string{"topology.topolvm.io/node": name},
		map[string]string{"capacity.topolvm.io/00default": "1073741824", "capacity.topolvm.io/ssd": "1073741824"})
}

func lvObj(name, node string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "topolvm.io/v1", "kind": "LogicalVolume",
		"metadata": map[string]any{"name": name, "uid": "uid-" + name},
		"spec":     map[string]any{"name": name, "nodeName": node, "size": "1Gi"},
		"status":   map[string]any{"volumeID": "uid-" + name, "currentSize": "1Gi"},
	}}
}

type fakes struct {
	core *fake.Clientset
	dyn  *dynamicfake.FakeDynamicClient
	meta *metadatafake.FakeMetadataClient
}

func (f fakes) clients() *Clients { return &Clients{Core: f.core, Dynamic: f.dyn, Meta: f.meta} }

func newFakes(nodes []runtime.Object, lvs []runtime.Object, core ...runtime.Object) fakes {
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		panic(err)
	}
	listKinds := map[schema.GroupVersionResource]string{
		topolvm.Current.GVR(): "LogicalVolumeList",
		topolvm.Legacy.GVR():  "LogicalVolumeList",
		lvmVolumeGroupGVR:     "LVMVolumeGroupList",
	}
	// Every TopoLVM install runs its controller in the TopoLVM namespace.
	controller := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "topolvm-controller-0", Namespace: "topolvm-system"}}
	return fakes{
		core: fake.NewClientset(append([]runtime.Object{controller}, core...)...),
		dyn:  dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, lvs...),
		meta: metadatafake.NewSimpleMetadataClient(scheme, nodes...),
	}
}

func forbidden(resource string) k8stesting.ReactionFunc {
	return func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("no"))
	}
}

func opts() CollectOptions {
	return CollectOptions{Driver: topolvm.Current, TopoLVMNamespace: "topolvm-system", Now: now}
}

func TestDetectDriver(t *testing.T) {
	ctx := context.Background()
	d, _, err := DetectDriver(ctx, fake.NewClientset(), "topolvm.cybozu.com")
	if err != nil || d != topolvm.Legacy {
		t.Errorf("flag: %v %v", d, err)
	}
	cur := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "topolvm.io"}}
	if d, _, err = DetectDriver(ctx, fake.NewClientset(cur), ""); err != nil || d != topolvm.Current {
		t.Errorf("current: %v %v", d, err)
	}
	leg := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "topolvm.cybozu.com"}}
	if d, _, err = DetectDriver(ctx, fake.NewClientset(leg), ""); err != nil || d != topolvm.Legacy {
		t.Errorf("legacy: %v %v", d, err)
	}
	if _, _, err = DetectDriver(ctx, fake.NewClientset(), ""); err == nil || !strings.Contains(err.Error(), "no TopoLVM CSIDriver found") {
		t.Errorf("none: %v", err)
	}
	cs := fake.NewClientset()
	cs.PrependReactor("get", "csidrivers", forbidden("csidrivers"))
	d, warnings, err := DetectDriver(ctx, cs, "")
	if err != nil || d != topolvm.Current || len(warnings) != 1 || warnings[0] != "cannot get csidrivers: forbidden; assuming topolvm.io" {
		t.Errorf("forbidden: %v %v %v", d, warnings, err)
	}
}

func TestCollectNodesByTopologyLabel(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a"), topoNode("b"), nodeMeta("c", nil, nil)}, nil)
	in, err := Collect(context.Background(), f.clients(), opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Nodes) != 2 || in.Nodes[0].Name != "a" || in.Nodes[1].Name != "b" || len(in.Warnings) != 0 {
		t.Errorf("nodes = %+v warnings = %q", in.Nodes, in.Warnings)
	}
}

func TestCollectNodesFallback(t *testing.T) {
	annotated := nodeMeta("x", nil, map[string]string{"capacity.topolvm.io/ssd": "5"})
	f := newFakes([]runtime.Object{annotated, nodeMeta("y", nil, nil)}, nil)
	in, err := Collect(context.Background(), f.clients(), opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Nodes) != 1 || in.Nodes[0].Name != "x" || in.Nodes[0].Capacity["ssd"] != 5 {
		t.Errorf("nodes = %+v", in.Nodes)
	}
}

func TestCollectNoNodes(t *testing.T) {
	f := newFakes([]runtime.Object{nodeMeta("y", nil, nil)}, nil)
	_, err := Collect(context.Background(), f.clients(), opts())
	if !errors.Is(err, ErrNoNodes) || !strings.Contains(err.Error(), "topology.topolvm.io/node") {
		t.Errorf("err = %v", err)
	}
}

func TestListAllFollowsContinue(t *testing.T) {
	calls := 0
	got, err := listAll(context.Background(), metav1.ListOptions{LabelSelector: "x"},
		func(_ context.Context, o metav1.ListOptions) ([]string, string, error) {
			calls++
			if calls > 3 {
				t.Fatal("pagination does not stop")
			}
			if o.Limit != pageSize || o.LabelSelector != "x" {
				t.Errorf("options = %+v", o)
			}
			switch o.Continue {
			case "":
				return []string{"a"}, "t1", nil
			case "t1":
				return []string{"b"}, "", nil
			default:
				t.Errorf("continue = %q", o.Continue)
				return nil, "", nil
			}
		})
	if err != nil || calls != 2 || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %v, calls %d, err %v", got, calls, err)
	}
}

func TestCollectAbsentNodes(t *testing.T) {
	unlabelled := nodeMeta("c", nil, nil)
	f := newFakes([]runtime.Object{topoNode("a"), unlabelled}, []runtime.Object{lvObj("lv1", "gone"), lvObj("lv2", "c"), lvObj("lv3", "a")})
	in, err := Collect(context.Background(), f.clients(), opts())
	if err != nil {
		t.Fatal(err)
	}
	if !in.AbsentNodes["gone"] || len(in.AbsentNodes) != 1 {
		t.Errorf("absent = %v", in.AbsentNodes)
	}
	if len(in.Nodes) != 2 || in.Nodes[1].Name != "c" || len(in.LVs) != 3 {
		t.Errorf("nodes = %+v, lvs = %d", in.Nodes, len(in.LVs))
	}
}

func TestCollectPVForbidden(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a")}, nil)
	f.core.PrependReactor("list", "persistentvolumes", forbidden("persistentvolumes"))
	o := opts()
	o.PVs = PVOptional
	in, err := Collect(context.Background(), f.clients(), o)
	if err != nil || in.PVs != nil || len(in.Warnings) != 1 ||
		in.Warnings[0] != "cannot list persistentvolumes: forbidden; PVC columns stay empty" {
		t.Errorf("optional: err=%v pvs=%v warnings=%q", err, in.PVs, in.Warnings)
	}
	o.PVs = PVRequired
	if _, err = Collect(context.Background(), f.clients(), o); err == nil || !strings.Contains(err.Error(), RBACHint) {
		t.Errorf("required: %v", err)
	}
}

func TestCollectPVsCollected(t *testing.T) {
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	f := newFakes([]runtime.Object{topoNode("a")}, nil, pv)
	o := opts()
	o.PVs = PVOptional
	in, err := Collect(context.Background(), f.clients(), o)
	if err != nil || len(in.PVs) != 1 {
		t.Errorf("pvs = %v, err = %v", in.PVs, err)
	}
	o.PVs = PVSkip
	if in, _ = Collect(context.Background(), f.clients(), o); in.PVs != nil {
		t.Errorf("PVSkip must not list PVs: %v", in.PVs)
	}
}

func TestCollectLVForbidden(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a")}, nil)
	f.dyn.PrependReactor("list", "logicalvolumes", forbidden("logicalvolumes"))
	_, err := Collect(context.Background(), f.clients(), opts())
	if err == nil || !strings.Contains(err.Error(), "logicalvolumes.topolvm.io") || !strings.Contains(err.Error(), "#rbac") {
		t.Errorf("err = %v", err)
	}
}

func TestCollectConfigMaps(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "topolvm-lvmd-0", Namespace: "topolvm-system", Labels: map[string]string{"app.kubernetes.io/name": "topolvm"}},
		Data:       map[string]string{"lvmd.yaml": "device-classes:\n  - name: ssd\n    volume-group: vg0\n    default: true\n"},
	}
	f := newFakes([]runtime.Object{topoNode("a")}, nil, cm)
	in, err := Collect(context.Background(), f.clients(), opts())
	if err != nil || in.Classes["ssd"].VolumeGroup != "vg0" {
		t.Errorf("classes = %v, err = %v", in.Classes, err)
	}
}

func TestCollectConfigMapForbidden(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a")}, nil)
	f.core.PrependReactor("list", "configmaps", forbidden("configmaps"))
	in, err := Collect(context.Background(), f.clients(), opts())
	want := []string{"cannot list configmaps in topolvm-system: forbidden; device class types and volume groups are guessed " +
		"(kubectl topolvm rbac prints the Role)"}
	if err != nil || in.Classes != nil || !reflect.DeepEqual(in.Warnings, want) {
		t.Errorf("classes = %v, warnings = %q, err = %v", in.Classes, in.Warnings, err)
	}
}

func TestCollectLVMSForbiddenWithoutLVMDConfig(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a")}, nil)
	f.dyn.PrependReactor("list", "lvmvolumegroups", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(lvmVolumeGroupGVR.GroupResource(), "", errors.New("no"))
	})
	in, err := Collect(context.Background(), f.clients(), opts())
	want := []string{"cannot list lvmvolumegroups.lvm.topolvm.io: forbidden; on Red Hat LVMS device class types are guessed " +
		"(kubectl topolvm rbac --with lvms prints the rule)"}
	if err != nil || !reflect.DeepEqual(in.Warnings, want) {
		t.Errorf("warnings = %q, err = %v", in.Warnings, err)
	}
}

func listedResources(actions []k8stesting.Action) map[string]int {
	out := map[string]int{}
	for _, a := range actions {
		if a.GetVerb() == "list" || a.GetVerb() == "get" {
			key := a.GetVerb() + " " + a.GetResource().Resource
			if a.GetResource().Resource == "pods" && a.GetNamespace() == "" {
				key += " -A"
			}
			out[key]++
		}
	}
	return out
}

func TestCollectNeedsOnlyWhatAsked(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a")}, nil)
	if _, err := Collect(context.Background(), f.clients(), opts()); err != nil {
		t.Fatal(err)
	}
	got := listedResources(f.core.Actions())
	for _, r := range []string{"list persistentvolumeclaims", "list pods -A", "list events", "list storageclasses",
		"list csistoragecapacities", "list csinodes", "get csidrivers", "list nodes"} {
		if got[r] != 0 {
			t.Errorf("base collect must not %s: %v", r, got)
		}
	}
	o := opts()
	o.Needs = NeedPVCs | NeedPods | NeedEvents | NeedStorage | NeedNodeStates | NeedTopoLVMPods
	f = newFakes([]runtime.Object{topoNode("a")}, nil, metricsPod("topolvm-node-a", "a", 8080))
	in, err := Collect(context.Background(), f.clients(), o)
	if err != nil {
		t.Fatal(err)
	}
	got = listedResources(f.core.Actions())
	for _, r := range []string{"list persistentvolumeclaims", "list storageclasses", "list csistoragecapacities",
		"list csinodes", "get csidrivers", "list nodes"} {
		if got[r] != 1 {
			t.Errorf("%s: %d calls (%v)", r, got[r], got)
		}
	}
	if got["list events"] != len(eventReasons) || got["list pods -A"] != 1 || got["list pods"] != 1 {
		t.Errorf("events and pods calls: %v", got)
	}
	if in.PVCs == nil || in.Pods == nil || in.Events == nil || in.StorageClasses == nil || in.Capacities == nil ||
		in.CSINodes == nil || in.NodeStates == nil || in.TopoLVMPods == nil {
		t.Errorf("collected fields must be non-nil: %+v", in)
	}
}

func TestCollectEventsByReason(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a")}, nil,
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "ns", UID: "e1"}, Reason: "FailedScheduling"})
	o := opts()
	o.Needs = NeedEvents
	in, err := Collect(context.Background(), f.clients(), o)
	if err != nil {
		t.Fatal(err)
	}
	var selectors []string
	for _, a := range f.core.Actions() {
		if l, ok := a.(k8stesting.ListAction); ok && a.GetResource().Resource == "events" {
			selectors = append(selectors, l.GetListRestrictions().Fields.String())
		}
	}
	want := []string{"reason=FailedScheduling", "reason=ProvisioningFailed", "reason=VolumeFailedDelete", "reason=VolumeResizeFailed"}
	sortStrings(selectors)
	if strings.Join(selectors, ",") != strings.Join(want, ",") {
		t.Errorf("selectors = %v", selectors)
	}
	if len(in.Events) != 1 {
		t.Errorf("events must be de-duplicated by uid: %d", len(in.Events))
	}
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func TestCollectOptionalForbidden(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a")}, nil)
	f.core.PrependReactor("list", "pods", forbidden("pods"))
	o := opts()
	o.Needs = NeedPods
	in, err := Collect(context.Background(), f.clients(), o)
	if err != nil || in.Pods != nil || len(in.Warnings) != 1 ||
		in.Warnings[0] != "cannot list pods: forbidden; consumers and owners of volumes are unknown" {
		t.Errorf("err=%v pods=%v warnings=%q", err, in.Pods, in.Warnings)
	}
}

func TestCollectNodeStates(t *testing.T) {
	ready := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Labels: map[string]string{"topology.topolvm.io/node": "a"}, Finalizers: []string{"topolvm.io/node"}},
		Spec:       corev1.NodeSpec{Unschedulable: true, Taints: []corev1.Taint{{Key: "k", Value: "v", Effect: corev1.TaintEffectNoSchedule}}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	f := newFakes([]runtime.Object{topoNode("a")}, nil, ready)
	o := opts()
	o.Needs = NeedNodeStates
	in, err := Collect(context.Background(), f.clients(), o)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := in.NodeStates["a"]
	if !ok || !s.Ready || !s.Unschedulable || !s.HasFinalizer || len(s.Taints) != 1 {
		t.Errorf("state = %+v", s)
	}
}

func TestCollectTopoLVMPods(t *testing.T) {
	node := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "topolvm-node-x", Namespace: "topolvm-system"},
		Spec: corev1.PodSpec{NodeName: "a", Containers: []corev1.Container{{Name: "topolvm-node"}, {Name: "csi-registrar"}}}}
	ctrl := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "topolvm-controller-y", Namespace: "topolvm-system"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "topolvm-controller"}}}}
	f := newFakes([]runtime.Object{topoNode("a")}, nil, node, ctrl)
	o := opts()
	o.Needs = NeedTopoLVMPods
	in, err := Collect(context.Background(), f.clients(), o)
	if err != nil || len(in.TopoLVMPods) != 1 || in.TopoLVMPods[0].Name != "topolvm-node-x" {
		t.Errorf("pods = %v, err = %v", in.TopoLVMPods, err)
	}
}

func TestCollectLVMVolumeGroupsFallback(t *testing.T) {
	vg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "lvm.topolvm.io/v1alpha1", "kind": "LVMVolumeGroup",
		"metadata": map[string]any{"name": "vg1", "namespace": "openshift-storage"},
		"spec":     map[string]any{"thinPoolConfig": map[string]any{"name": "p", "overprovisionRatio": int64(10)}},
	}}
	f := newFakes([]runtime.Object{topoNode("a")}, []runtime.Object{vg})
	in, err := Collect(context.Background(), f.clients(), opts())
	if err != nil || in.Classes["vg1"].Type != "thin" || !in.Classes["vg1"].Default {
		t.Errorf("classes = %+v, err = %v", in.Classes, err)
	}
}

func TestCollectTopoLVMPodsWrongNamespace(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a")}, nil)
	o := opts()
	o.TopoLVMNamespace = "elsewhere"
	o.Needs = NeedTopoLVMPods
	in, err := Collect(context.Background(), f.clients(), o)
	if err != nil {
		t.Fatal(err)
	}
	if in.TopoLVMPods != nil {
		t.Errorf("no topolvm-node pods must leave TopoLVMPods nil, got %v", in.TopoLVMPods)
	}
	want := "no pods in namespace elsewhere; pass --topolvm-namespace (lvmd config, topolvm-node readiness and --metrics are skipped)"
	if len(in.Warnings) != 1 || in.Warnings[0] != want {
		t.Errorf("warnings = %q", in.Warnings)
	}
	o.Needs = 0
	if in, err = Collect(context.Background(), f.clients(), o); err != nil || len(in.Warnings) != 1 || in.Warnings[0] != want {
		t.Errorf("pools must say the namespace is empty too: %q %v", in.Warnings, err)
	}
}

func TestCollectSkipsUnparsableLV(t *testing.T) {
	bad := lvObj("bad", "a")
	bad.Object["spec"].(map[string]any)["size"] = "ten"
	f := newFakes([]runtime.Object{topoNode("a")}, []runtime.Object{lvObj("good", "a"), bad})
	in, err := Collect(context.Background(), f.clients(), opts())
	if err != nil {
		t.Fatalf("one bad LogicalVolume must not fail the command: %v", err)
	}
	if len(in.LVs) != 1 || in.LVs[0].Name != "good" || len(in.Warnings) != 1 || !strings.Contains(in.Warnings[0], "logicalvolume bad") {
		t.Errorf("lvs = %v warnings = %q", in.LVs, in.Warnings)
	}
	if !reflect.DeepEqual(in.SkippedLVs, []string{"uid-bad"}) {
		t.Errorf("the skipped LV's volume ID keeps its PV out of pv-without-lv: %v", in.SkippedLVs)
	}
}

func TestCollectKeepsConfigMapWarningsOnFallback(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "topolvm-lvmd-0", Namespace: "topolvm-system", Labels: map[string]string{"app.kubernetes.io/name": "topolvm"}},
		Data:       map[string]string{"lvmd.yaml": "device-classes: [: :"},
	}
	vg := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "lvm.topolvm.io/v1alpha1", "kind": "LVMVolumeGroup",
		"metadata": map[string]any{"name": "vg1", "namespace": "openshift-storage"}, "spec": map[string]any{},
	}}
	f := newFakes([]runtime.Object{topoNode("a")}, []runtime.Object{vg}, cm)
	in, err := Collect(context.Background(), f.clients(), opts())
	if err != nil || in.Classes["vg1"].Type != "thick" || len(in.Warnings) != 1 || !strings.Contains(in.Warnings[0], "cannot parse lvmd.yaml") {
		t.Errorf("classes = %v warnings = %q err = %v", in.Classes, in.Warnings, err)
	}
}

func TestCollectNodeClasses(t *testing.T) {
	cmA := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "topolvm-lvmd-0", Namespace: "topolvm-system", Labels: map[string]string{"app.kubernetes.io/name": "topolvm"}},
		Data:       map[string]string{"lvmd.yaml": "device-classes:\n  - name: ssd\n    volume-group: vg-a\n    default: true\n"},
	}
	cmB := cmA.DeepCopy()
	cmB.Name = "topolvm-lvmd-1"
	cmB.Data["lvmd.yaml"] = "device-classes:\n  - name: ssd\n    volume-group: vg-b\n    default: true\n"
	pod := func(name, node, cm string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "topolvm-system"},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "lvmd"}}, Volumes: []corev1.Volume{{Name: "config",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cm}}}}}},
		}
	}
	f := newFakes([]runtime.Object{topoNode("a"), topoNode("b")}, nil, cmA, cmB, pod("l-a", "a", "topolvm-lvmd-0"), pod("l-b", "b", "topolvm-lvmd-1"))
	in, err := Collect(context.Background(), f.clients(), opts())
	if err != nil {
		t.Fatal(err)
	}
	if in.NodeClasses["a"]["ssd"].VolumeGroup != "vg-a" || in.NodeClasses["b"]["ssd"].VolumeGroup != "vg-b" || in.Classes["ssd"].Type != "thick" {
		t.Errorf("node classes = %+v classes = %+v", in.NodeClasses, in.Classes)
	}
	if in.TopoLVMPods != nil {
		t.Errorf("TopoLVMPods are only filled when asked for: %v", in.TopoLVMPods)
	}
}

func TestCollectSeesSchedulerExtender(t *testing.T) {
	sched := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "topolvm-scheduler-x", Namespace: "topolvm-system"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "topolvm-scheduler"}}}}
	in, err := Collect(context.Background(), newFakes([]runtime.Object{topoNode("a")}, nil, sched).clients(), opts())
	if err != nil || !in.SchedulerExtender {
		t.Errorf("extender = %v, err = %v", in.SchedulerExtender, err)
	}
	if in, _ = Collect(context.Background(), newFakes([]runtime.Object{topoNode("a")}, nil).clients(), opts()); in.SchedulerExtender {
		t.Error("no topolvm-scheduler pod, no extender")
	}
}

func TestCollectFindsLVMDConfigWithoutChartLabels(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "lvmd-renamed", Namespace: "topolvm-system",
		Labels: map[string]string{"app.kubernetes.io/name": "storage"}},
		Data: map[string]string{"lvmd.yaml": "device-classes:\n  - name: ssd\n    volume-group: vg0\n    default: true\n"}}
	in, err := Collect(context.Background(), newFakes([]runtime.Object{topoNode("a")}, nil, cm).clients(), opts())
	if err != nil || in.Classes["ssd"].VolumeGroup != "vg0" {
		t.Errorf("a chart with nameOverride labels its ConfigMaps differently: classes = %+v, err = %v", in.Classes, err)
	}
}

func TestCollectKeepsAnnotatedNodesWithoutTheLabel(t *testing.T) {
	unlabelled := nodeMeta("b", nil, map[string]string{"capacity.topolvm.io/ssd": "1073741824"})
	in, err := Collect(context.Background(), newFakes([]runtime.Object{topoNode("a"), unlabelled}, nil).clients(), opts())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(in.Nodes))
	for _, n := range in.Nodes {
		names = append(names, n.Name)
	}
	want := "node b has TopoLVM capacity annotations but no topology.topolvm.io/node label: the driver is not registered there, the scheduler cannot place volumes on it"
	if !reflect.DeepEqual(names, []string{"a", "b"}) || !slices.Contains(in.Warnings, want) {
		t.Errorf("nodes = %v warnings = %q", names, in.Warnings)
	}
}

func TestNodeStateReadsReadinessAndTheDriversFinalizer(t *testing.T) {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Finalizers: []string{"topolvm.cybozu.com/node"}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	if s := nodeState(n, topolvm.Legacy); !s.Ready || !s.HasFinalizer {
		t.Errorf("legacy = %+v", s)
	}
	if s := nodeState(n, topolvm.Current); s.HasFinalizer {
		t.Errorf("topolvm.io does not own topolvm.cybozu.com/node: %+v", s)
	}
	n.Finalizers = []string{"topolvm.io/node"}
	n.Status.Conditions[0].Status = corev1.ConditionUnknown
	if s := nodeState(n, topolvm.Current); s.Ready || !s.HasFinalizer {
		t.Errorf("current = %+v", s)
	}
}

func TestCollectNodeGetErrorIsNotAbsence(t *testing.T) {
	f := newFakes([]runtime.Object{topoNode("a")}, []runtime.Object{lvObj("lv1", "x")})
	f.meta.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd timeout"))
	})
	in, err := Collect(context.Background(), f.clients(), opts())
	if err != nil {
		t.Fatal(err)
	}
	if in.AbsentNodes["x"] || len(in.Warnings) != 1 || !strings.HasPrefix(in.Warnings[0], "cannot get node x: ") {
		t.Errorf("a failed GET is not a deleted node: absent = %v warnings = %q", in.AbsentNodes, in.Warnings)
	}
}

func TestListAllRestartsWhenTheContinueTokenExpires(t *testing.T) {
	calls := 0
	items, err := listAll(context.Background(), metav1.ListOptions{}, func(_ context.Context, o metav1.ListOptions) ([]string, string, error) {
		calls++
		switch {
		case o.Continue == "" && o.Limit > 0:
			return []string{"a"}, "next", nil
		case o.Continue == "next":
			return nil, "", apierrors.NewResourceExpired("continue token expired")
		default:
			return []string{"a", "b", "c"}, "", nil
		}
	})
	if err != nil || !reflect.DeepEqual(items, []string{"a", "b", "c"}) || calls != 3 {
		t.Errorf("items = %v, calls = %d, err = %v", items, calls, err)
	}
}

func TestNodeStatesWithoutPerNodeGets(t *testing.T) {
	unlabelled := nodeMeta("b", nil, map[string]string{"capacity.topolvm.io/ssd": "1073741824"})
	f := newFakes([]runtime.Object{topoNode("a"), unlabelled}, nil,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a", Labels: map[string]string{"topology.topolvm.io/node": "a"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "b", Finalizers: []string{"topolvm.io/node"}}})
	o := opts()
	o.Needs = NeedNodeStates
	in, err := Collect(context.Background(), f.clients(), o)
	if err != nil || len(in.NodeStates) != 2 || !in.NodeStates["b"].HasFinalizer {
		t.Fatalf("states = %+v, err = %v", in.NodeStates, err)
	}
	if got := listedResources(f.core.Actions())["get nodes"]; got != 0 {
		t.Errorf("node states come from one list, got %d GETs", got)
	}
}
