package report_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

func pairs(fs []report.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Check+" "+f.Object)
	}
	return out
}

func TestChecksAnomalies(t *testing.T) {
	r := build(load(t, "anomalies"))
	want := []string{
		"orphan-lv logicalvolume/pvc-a1",
		"pv-without-lv persistentvolume/pvc-a4",
		"released-pv persistentvolume/pvc-a5",
		"lv-on-missing-node logicalvolume/pvc-a6",
		"lv-on-missing-node logicalvolume/pvc-a8",
		"stuck-deletion logicalvolume/pvc-a7",
		"pending-deletion logicalvolume/pvc-a8",
		"lv-error logicalvolume/pvc-a9",
		"lv-error logicalvolume/pvc-a10",
		"resize-pending logicalvolume/pvc-a10",
		"uid-mismatch logicalvolume/pvc-a12",
	}
	if got := pairs(r.Findings); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	byKey := map[string]report.Finding{}
	for _, f := range r.Findings {
		byKey[f.Check+" "+f.Object] = f
	}
	if f := byKey["stuck-deletion logicalvolume/pvc-a7"]; i64(f.AgeSeconds) != "7200" {
		t.Errorf("stuck-deletion age = %s", i64(f.AgeSeconds))
	}
	if f := byKey["pv-without-lv persistentvolume/pvc-a4"]; f.Node != "node-b" || f.SizeBytes != nil ||
		f.Detail != "no LogicalVolume with volumeID uid-missing" {
		t.Errorf("pv-without-lv = %+v", f)
	}
	if f := byKey["released-pv persistentvolume/pvc-a5"]; i64(f.SizeBytes) != "21474836480" || f.AgeSeconds != nil ||
		f.Detail != "PV is Released (reclaimPolicy Retain), the LV still holds the space" {
		t.Errorf("released-pv = %+v", f)
	}
	if f := byKey["lv-error logicalvolume/pvc-a9"]; f.Detail != "create of 4Gi failed: ResourceExhausted: not enough space" {
		t.Errorf("create failure detail = %q", f.Detail)
	}
	if f := byKey["lv-error logicalvolume/pvc-a10"]; f.Detail != "expand failed: Internal: not enough space" {
		t.Errorf("expand failure detail = %q", f.Detail)
	}
	if f := byKey["resize-pending logicalvolume/pvc-a10"]; f.Detail != "spec.size 30Gi, current 20Gi" || i64(f.AgeSeconds) != "10799" {
		t.Errorf("resize-pending = %+v age=%s", f, i64(f.AgeSeconds))
	}
	if f := byKey["lv-on-missing-node logicalvolume/pvc-a6"]; f.Node != "node-gone" || f.Detail != "node node-gone no longer exists" {
		t.Errorf("lv-on-missing-node = %+v", f)
	}
	if f := byKey["uid-mismatch logicalvolume/pvc-a12"]; f.Detail !=
		"status.volumeID u-old differs from metadata.uid u-new: the CR was recreated" {
		t.Errorf("uid-mismatch detail = %q", f.Detail)
	}
	if f := byKey["stuck-deletion logicalvolume/pvc-a7"]; f.Detail != "deletion requested 120m ago, finalizers: topolvm.io/logicalvolume" {
		t.Errorf("stuck-deletion detail = %q", f.Detail)
	}
	if f := byKey["pending-deletion logicalvolume/pvc-a8"]; f.Detail !=
		"topolvm.io/pendingdeletion is set but finalizer topolvm.io/logicalvolume is still there" {
		t.Errorf("pending-deletion detail = %q", f.Detail)
	}
	if f := byKey["orphan-lv logicalvolume/pvc-a1"]; f.Detail != "no PersistentVolume with volumeHandle 00000000-0000-0000-0000-000000000001" ||
		i64(f.SizeBytes) != "10737418240" {
		t.Errorf("orphan-lv = %+v", f)
	}
}

func TestChecksSkip(t *testing.T) {
	r := report.Build(load(t, "anomalies"), report.Options{MinAge: 15 * time.Minute, Checks: true, Skip: []string{"released-pv", "lv-error"}})
	if len(r.Findings) != 8 {
		t.Errorf("findings = %v", pairs(r.Findings))
	}
}

func TestChecksNodeFilter(t *testing.T) {
	r := report.Build(load(t, "anomalies"), report.Options{MinAge: 15 * time.Minute, Checks: true, Nodes: []string{"node-gone"}})
	if got := pairs(r.Findings); !reflect.DeepEqual(got, []string{
		"lv-on-missing-node logicalvolume/pvc-a6", "lv-on-missing-node logicalvolume/pvc-a8", "pending-deletion logicalvolume/pvc-a8"}) {
		t.Errorf("findings = %v", got)
	}
}

func TestChecksCleanFixtures(t *testing.T) {
	for _, name := range []string{"single-thick", "multi-class", "legacy"} {
		if r := build(load(t, name)); len(r.Findings) != 0 {
			t.Errorf("%s: findings = %v", name, pairs(r.Findings))
		}
	}
}

func TestChecksNoPVs(t *testing.T) {
	in := load(t, "anomalies")
	in.PVs = nil
	r := build(in)
	for _, f := range r.Findings {
		if f.Check == "orphan-lv" || f.Check == "pv-without-lv" || f.Check == "released-pv" {
			t.Errorf("PV check ran without PVs: %+v", f)
		}
	}
	want := "persistentvolumes not collected: orphan-lv, pv-without-lv and released-pv skipped"
	found := false
	for _, w := range r.Warnings {
		found = found || w == want
	}
	if !found {
		t.Errorf("warnings = %q", r.Warnings)
	}
}

func TestChecksOffByDefault(t *testing.T) {
	r := report.Build(load(t, "anomalies"), report.Options{MinAge: 15 * time.Minute})
	if len(r.Findings) != 0 {
		t.Errorf("findings without Checks: %v", pairs(r.Findings))
	}
	in := load(t, "anomalies")
	in.PVs = nil
	if r := report.Build(in, report.Options{}); len(r.Warnings) != 0 {
		t.Errorf("warnings without Checks: %q", r.Warnings)
	}
}

func TestCheckIDsOrder(t *testing.T) {
	want := []string{"orphan-lv", "pv-without-lv", "released-pv", "lv-on-missing-node",
		"stuck-deletion", "pending-deletion", "lv-error", "resize-pending", "uid-mismatch"}
	if !reflect.DeepEqual(report.CheckIDs[:len(want)], want) {
		t.Errorf("CheckIDs = %v", report.CheckIDs)
	}
}

func TestChecksDeviceClassAndNamespaceFilter(t *testing.T) {
	r := report.Build(load(t, "anomalies"), report.Options{MinAge: 15 * time.Minute, Checks: true, Namespace: "ns-x"})
	for _, f := range r.Findings {
		if f.Namespace != "ns-x" {
			t.Errorf("-n ns-x kept a finding from %q: %+v", f.Namespace, f)
		}
	}
	if len(r.Findings) == 0 {
		t.Error("-n ns-x must keep the findings of claims in ns-x")
	}
	r = report.Build(load(t, "anomalies"), report.Options{MinAge: 15 * time.Minute, Checks: true, DeviceClasses: []string{"ssd"}})
	for _, f := range r.Findings {
		if f.DeviceClass != "ssd" {
			t.Errorf("--device-class ssd kept %+v", f)
		}
	}
	r = report.Build(load(t, "anomalies"), report.Options{MinAge: 15 * time.Minute, Checks: true, DeviceClasses: []string{"nvme"}})
	if len(r.Findings) != 0 {
		t.Errorf("--device-class nvme: %v", pairs(r.Findings))
	}
}

func TestReleasedAndReclaimAgeGate(t *testing.T) {
	in := load(t, "anomalies")
	recent := metav1.NewTime(in.Now.Add(-time.Minute))
	for i := range in.PVs {
		if in.PVs[i].Name == "pvc-a5" {
			in.PVs[i].Status.LastPhaseTransitionTime = &recent
		}
	}
	for _, f := range build(in).Findings {
		if f.Check == "released-pv" {
			t.Errorf("a PV released a minute ago may still be reclaimed: %+v", f)
		}
	}
	in = load(t, "anomalies")
	for i := range in.PVs {
		if in.PVs[i].Name == "pvc-a4" {
			in.PVs[i].Status.Phase = corev1.VolumeReleased
			in.PVs[i].Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
			in.PVs[i].Status.LastPhaseTransitionTime = &recent
		}
	}
	for _, f := range build(in).Findings {
		if f.Check == "pv-without-lv" {
			t.Errorf("a Delete PV whose LV was just removed is being reclaimed: %+v", f)
		}
	}
}

func TestPendingDeletionOnALiveNode(t *testing.T) {
	in := load(t, "anomalies")
	for i := range in.LVs {
		if in.LVs[i].Name == "pvc-a8" {
			in.LVs[i].NodeName = "node-a"
		}
	}
	found := false
	for _, f := range build(in).Findings {
		found = found || (f.Check == "pending-deletion" && f.Object == "logicalvolume/pvc-a8")
	}
	if !found {
		t.Error("the controller marks an LV and drops its finalizer in one patch; marked with the finalizer, topolvm-node skips it forever")
	}
}

func TestNameFallbackSkipsMatchedPV(t *testing.T) {
	in := report.Input{
		Driver: topolvm.Current,
		Nodes:  oneNode(),
		LVs: []topolvm.LogicalVolume{
			{Name: "lv-a", UID: "ka", VolumeID: "ka", NodeName: "n", SizeBytes: gi},
			{Name: "pv-x", UID: "kb", VolumeID: "kb", NodeName: "n", SizeBytes: gi},
		},
		PVs: []corev1.PersistentVolume{testPV("pv-x", "ka", corev1.VolumeBound)},
	}
	r := report.Build(in, report.Options{})
	for _, v := range r.Volumes {
		if v.LV == "pv-x" && v.Status != "Orphan" {
			t.Errorf("pv-x is already matched to lv-a by handle; lv-pv-x must be an orphan: %+v", v)
		}
	}
}

func TestReleasedDeletePV(t *testing.T) {
	in := load(t, "anomalies")
	released := metav1.NewTime(in.Now.Add(-2 * time.Hour))
	var handle string
	for i := range in.PVs {
		if in.PVs[i].Name == "pvc-a5" {
			in.PVs[i].Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
			in.PVs[i].Status.LastPhaseTransitionTime = &released
			handle = in.PVs[i].Spec.CSI.VolumeHandle
		}
	}
	in.Events = append(in.Events, corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolume", Name: "pvc-a5"},
		Reason:         "VolumeFailedDelete", Message: "rpc error: code = DeadlineExceeded desc = context deadline exceeded",
		LastTimestamp: metav1.NewTime(in.Now.Add(-time.Minute)),
	})
	find := func(r report.Report, check string) report.Finding {
		for _, f := range r.Findings {
			if f.Check == check && (f.Object == "persistentvolume/pvc-a5" || f.Object == "logicalvolume/"+lvOf(in, handle)) {
				return f
			}
		}
		t.Fatalf("no %s for pvc-a5 in %v", check, pairs(r.Findings))
		return report.Finding{}
	}
	f := find(build(in), "released-pv")
	if f.Detail != "PV is Released (reclaimPolicy Delete) and was not deleted, the LV still holds the space; "+
		"last error: rpc error: code = DeadlineExceeded desc = context deadline exceeded" ||
		i64(f.SizeBytes) != "21474836480" || i64(f.AgeSeconds) != "7200" {
		t.Errorf("released-pv = %+v", f)
	}

	deleted := in.Now.Add(-time.Hour)
	for i := range in.LVs {
		if in.LVs[i].Key() == handle {
			in.LVs[i].DeletionTimestamp = &deleted
			in.LVs[i].Finalizers = []string{"e2e.topolvm/hold"}
		}
	}
	r := build(in)
	if f = find(r, "released-pv"); !strings.HasPrefix(f.Detail, "PV is Released (reclaimPolicy Delete) and was not deleted, the LV is already removed from disk;") ||
		f.SizeBytes != nil {
		t.Errorf("released-pv after the disk LV is gone = %+v", f)
	}
	if f = find(r, "stuck-deletion"); f.Detail != "deletion requested 60m ago, finalizers: e2e.topolvm/hold; the LV is already removed from disk" ||
		f.SizeBytes != nil {
		t.Errorf("stuck-deletion after the disk LV is gone = %+v", f)
	}
}

func lvOf(in report.Input, handle string) string {
	for _, lv := range in.LVs {
		if lv.Key() == handle {
			return lv.Name
		}
	}
	return ""
}

func TestPVsJoinOnlyByVolumeID(t *testing.T) {
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	in := report.Input{
		Driver: topolvm.Current,
		Now:    time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
		Nodes:  oneNode(),
		LVs:    []topolvm.LogicalVolume{{Name: "pvc-x", UID: "u-new", VolumeID: "u-new", NodeName: "n", SizeBytes: gi, Created: old}},
		PVs:    []corev1.PersistentVolume{testPV("pvc-x", "u-old", corev1.VolumeBound)},
	}
	in.PVs[0].CreationTimestamp = metav1.NewTime(old)
	r := report.Build(in, report.Options{MinAge: 15 * time.Minute, Checks: true})
	if r.Volumes[0].Status != "Orphan" || r.Volumes[0].PV != nil {
		t.Errorf("TopoLVM resolves a PV only by volumeID, never by name: %+v", r.Volumes[0])
	}
	want := map[string]string{
		"pv-without-lv persistentvolume/pvc-x": "no LogicalVolume with volumeID u-old; LogicalVolume pvc-x has volumeID u-new, " +
			"and TopoLVM finds volumes only by volumeID",
		"orphan-lv logicalvolume/pvc-x": "no PersistentVolume with volumeHandle u-new",
	}
	got := map[string]string{}
	for _, f := range r.Findings {
		got[f.Check+" "+f.Object] = f.Detail
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %q", got)
	}
}

func TestNoDefaultClassCheck(t *testing.T) {
	in := report.Input{
		Driver:  topolvm.Current,
		Nodes:   []topolvm.Node{{Name: "n", Capacity: map[string]int64{"00default": 0, "ssd": 50 * gi}}},
		Classes: map[string]topolvm.DeviceClass{"ssd": {Name: "ssd", VolumeGroup: "vg0", Type: topolvm.TypeThick}},
		StorageClasses: []storagev1.StorageClass{
			{ObjectMeta: metav1.ObjectMeta{Name: "plain"}, Provisioner: "topolvm.io", VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer)},
			{ObjectMeta: metav1.ObjectMeta{Name: "fast"}, Provisioner: "topolvm.io", VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer),
				Parameters: map[string]string{"topolvm.io/device-class": "ssd"}},
		},
	}
	r := report.Build(in, report.Options{Checks: true})
	want := []string{"no-default-class storageclass/plain"}
	if got := pairs(r.Findings); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v", got)
	}
	if r.Findings[0].Detail != "no device-class parameter and no node has a default device class: its claims cannot be provisioned" {
		t.Errorf("detail = %q", r.Findings[0].Detail)
	}
	in.Classes["ssd"] = topolvm.DeviceClass{Name: "ssd", VolumeGroup: "vg0", Type: topolvm.TypeThick, Default: true}
	if r = report.Build(in, report.Options{Checks: true}); len(r.Findings) != 0 {
		t.Errorf("with a default class: %v", pairs(r.Findings))
	}
}

func TestStorageCapacityOffWithSchedulerExtender(t *testing.T) {
	off := false
	in := report.Input{Driver: topolvm.Current, Nodes: oneNode(), CSIDriver: &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: "topolvm.io"}, Spec: storagev1.CSIDriverSpec{StorageCapacity: &off}}}
	if got := pairs(report.Build(in, report.Options{Checks: true}).Findings); !reflect.DeepEqual(got, []string{"storage-capacity-disabled csidriver/topolvm.io"}) {
		t.Errorf("without topolvm-scheduler: %v", got)
	}
	in.SchedulerExtender = true
	if got := report.Build(in, report.Options{Checks: true}).Findings; len(got) != 0 {
		t.Errorf("topolvm-scheduler filters by the capacity annotations itself: %v", pairs(got))
	}
}

func TestResizeStuckSkipsFilesystemWaitingForAMount(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cur := 20 * gi
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "ns", CreationTimestamp: metav1.NewTime(now.AddDate(0, -1, 0))},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "p",
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			Conditions: []corev1.PersistentVolumeClaimCondition{{Type: corev1.PersistentVolumeClaimFileSystemResizePending,
				Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now.Add(-time.Hour))}}},
	}
	in := report.Input{
		Driver: topolvm.Current, Now: now, Nodes: oneNode(),
		LVs:  []topolvm.LogicalVolume{{Name: "p", UID: "u", VolumeID: "u", NodeName: "n", SizeBytes: 20 * gi, CurrentSizeBytes: &cur, Created: now.AddDate(0, -1, 0)}},
		PVs:  []corev1.PersistentVolume{testPV("p", "u", corev1.VolumeBound)},
		PVCs: []corev1.PersistentVolumeClaim{pvc},
		Pods: []corev1.Pod{},
	}
	stuck := func() bool {
		for _, f := range report.Build(in, report.Options{MinAge: 15 * time.Minute, Checks: true}).Findings {
			if f.Check == "pvc-resize-stuck" {
				return true
			}
		}
		return false
	}
	if stuck() {
		t.Error("the LV is expanded and kubelet grows the filesystem on the next mount")
	}
	in.Pods = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"}, Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}}}}
	if !stuck() {
		t.Error("a mounted filesystem that does not grow for an hour is stuck")
	}
	in.PVCs[0].Status.Conditions = nil
	in.LVs[0].CurrentSizeBytes = new(10 * gi)
	in.LVs[0].Annotations = map[string]string{"topolvm.io/resize-requested-at": now.Add(-time.Minute).Format("2006-01-02 15:04:05.999999999 -0700 MST")}
	if stuck() {
		t.Error("a resize requested a minute ago on an old claim is not stuck yet")
	}
}

func TestChecksIgnoreOtherDrivers(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	old := metav1.NewTime(now.AddDate(0, -1, 0))
	ebs := testPV("ebs-1", "vol-0abc", corev1.VolumeBound)
	ebs.Spec.CSI.Driver = "ebs.csi.aws.com"
	ebs.CreationTimestamp = old
	released := testPV("ebs-2", "vol-0def", corev1.VolumeReleased)
	released.Spec.CSI.Driver = "ebs.csi.aws.com"
	released.CreationTimestamp = old
	in := report.Input{
		Driver: topolvm.Current, Now: now, Nodes: oneNode(),
		PVs: []corev1.PersistentVolume{ebs, released},
		PVCs: []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{Name: "claim-ebs-1", Namespace: "ns", CreationTimestamp: old},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "ebs-1", StorageClassName: new("gp3"),
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}}},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
		}, {
			ObjectMeta: metav1.ObjectMeta{Name: "waiting", Namespace: "ns", CreationTimestamp: old},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: new("gp3")},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		}},
		Pods:           []corev1.Pod{},
		Events:         []corev1.Event{},
		StorageClasses: []storagev1.StorageClass{{ObjectMeta: metav1.ObjectMeta{Name: "gp3", CreationTimestamp: old}, Provisioner: "ebs.csi.aws.com"}},
		Capacities:     []storagev1.CSIStorageCapacity{},
		CSINodes: []storagev1.CSINode{{ObjectMeta: metav1.ObjectMeta{Name: "other", CreationTimestamp: old},
			Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: "ebs.csi.aws.com", NodeID: "i-1"}}}}},
	}
	if f := report.Build(in, report.Options{MinAge: 15 * time.Minute, Checks: true}).Findings; len(f) != 0 {
		t.Errorf("objects of other CSI drivers are not TopoLVM's business: %v", pairs(f))
	}
}

func TestCheckGates(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, -1, 0)
	at := func(d time.Duration) string { return now.Add(-d).Format("2006-01-02 15:04:05.999999999 -0700 MST") }
	lv := func() topolvm.LogicalVolume {
		return topolvm.LogicalVolume{Name: "lv", UID: "u", VolumeID: "u", NodeName: "n", SizeBytes: 10 * gi, Created: old}
	}
	wffc := storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "topolvm-ssd", CreationTimestamp: metav1.NewTime(old)},
		Provisioner: "topolvm.io", VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer)}
	pending := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "waiting", Namespace: "ns", CreationTimestamp: metav1.NewTime(old)},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: new("topolvm-ssd"),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	consumer := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns", CreationTimestamp: metav1.NewTime(old)},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "waiting"}}}}}}
	cases := []struct {
		name   string
		mutate func(*report.Input)
		check  string
		want   bool
	}{
		{"resize requested a minute ago", func(in *report.Input) {
			in.LVs[0].CurrentSizeBytes = new(5 * gi)
			in.LVs[0].Annotations = map[string]string{"topolvm.io/resize-requested-at": at(time.Minute)}
		}, "resize-pending", false},
		{"resize requested an hour ago", func(in *report.Input) {
			in.LVs[0].CurrentSizeBytes = new(5 * gi)
			in.LVs[0].Annotations = map[string]string{"topolvm.io/resize-requested-at": at(time.Hour)}
		}, "resize-pending", true},
		{"resize without a request time", func(in *report.Input) { in.LVs[0].CurrentSizeBytes = new(5 * gi) }, "resize-pending", true},
		{"deleted a minute ago", func(in *report.Input) { in.LVs[0].DeletionTimestamp = new(now.Add(-time.Minute)) }, "stuck-deletion", false},
		{"deleted an hour ago", func(in *report.Input) { in.LVs[0].DeletionTimestamp = new(now.Add(-time.Hour)) }, "stuck-deletion", true},
		{"orphan being deleted", func(in *report.Input) { in.LVs[0].DeletionTimestamp = new(now.Add(-time.Hour)) }, "orphan-lv", false},
		{"orphan", func(*report.Input) {}, "orphan-lv", true},
		{"orphan created a minute ago", func(in *report.Input) { in.LVs[0].Created = now.Add(-time.Minute) }, "orphan-lv", false},
		{"pending without a consumer", func(in *report.Input) { in.PVCs = []corev1.PersistentVolumeClaim{pending} }, "pvc-pending", false},
		{"pending with a consumer", func(in *report.Input) {
			in.PVCs, in.Pods = []corev1.PersistentVolumeClaim{pending}, []corev1.Pod{consumer}
		}, "pvc-pending", true},
	}
	for _, c := range cases {
		in := report.Input{Driver: topolvm.Current, Now: now, Nodes: oneNode(), LVs: []topolvm.LogicalVolume{lv()},
			PVs: []corev1.PersistentVolume{}, PVCs: []corev1.PersistentVolumeClaim{}, Pods: []corev1.Pod{},
			StorageClasses: []storagev1.StorageClass{wffc}}
		c.mutate(&in)
		got := false
		for _, f := range report.Build(in, report.Options{MinAge: 15 * time.Minute, Checks: true}).Findings {
			got = got || f.Check == c.check
		}
		if got != c.want {
			t.Errorf("%s: %s reported = %v, want %v", c.name, c.check, got, c.want)
		}
	}
}

func TestSkippedLVKeepsItsPV(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	pv := testPV("p", "k", corev1.VolumeBound)
	pv.CreationTimestamp = metav1.NewTime(now.AddDate(0, -1, 0))
	in := report.Input{Driver: topolvm.Current, Now: now, Nodes: oneNode(), PVs: []corev1.PersistentVolume{pv}, SkippedLVs: []string{"k"}}
	if f := report.Build(in, report.Options{MinAge: 15 * time.Minute, Checks: true}).Findings; len(f) != 0 {
		t.Errorf("a LogicalVolume that exists but could not be read is not missing: %v", pairs(f))
	}
}

func TestNoDefaultClassOnlyWhenProven(t *testing.T) {
	plain := storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "plain"}, Provisioner: "topolvm.io",
		VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer)}
	for _, c := range []struct {
		name     string
		capacity map[string]int64
		classes  map[string]topolvm.DeviceClass
		want     bool
	}{
		{"config without a default", map[string]int64{"00default": 0, "ssd": 50 * gi}, map[string]topolvm.DeviceClass{"ssd": {Name: "ssd"}}, true},
		{"merged config lost the default, alias says one exists", map[string]int64{"00default": 50 * gi, "ssd": 50 * gi},
			map[string]topolvm.DeviceClass{"ssd": {Name: "ssd"}}, false},
		{"no config, alias 0 and no full class", map[string]int64{"00default": 0, "ssd": 50 * gi, "hdd": 50 * gi}, nil, true},
		{"no config, alias equal to two classes", map[string]int64{"00default": 50 * gi, "ssd": 50 * gi, "hdd": 50 * gi}, nil, false},
		{"no config, alias 0 and a full class", map[string]int64{"00default": 0, "ssd": 0, "hdd": 50 * gi}, nil, false},
		{"no config, no alias", map[string]int64{"ssd": 50 * gi, "hdd": 50 * gi}, nil, false},
	} {
		in := report.Input{Driver: topolvm.Current, Nodes: []topolvm.Node{{Name: "n", Capacity: c.capacity}}, Classes: c.classes,
			StorageClasses: []storagev1.StorageClass{plain}}
		got := false
		for _, f := range report.Build(in, report.Options{Checks: true}).Findings {
			got = got || f.Check == "no-default-class"
		}
		if got != c.want {
			t.Errorf("%s: no-default-class = %v, want %v", c.name, got, c.want)
		}
	}
}
