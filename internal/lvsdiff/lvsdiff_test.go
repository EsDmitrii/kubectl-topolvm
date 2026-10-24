package lvsdiff

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

const (
	gi    = int64(1) << 30
	uidOK = "11111111-1111-1111-1111-111111111111"
	uidSm = "22222222-2222-2222-2222-222222222222"
	uidCR = "33333333-3333-3333-3333-333333333333"
	uidDk = "44444444-4444-4444-4444-444444444444"
)

func input() report.Input {
	small := 10 * gi
	return report.Input{
		Driver:  topolvm.Current,
		Classes: map[string]topolvm.DeviceClass{"ssd": {Name: "ssd", VolumeGroup: "vg0", Type: topolvm.TypeThick, Default: true}},
		LVs: []topolvm.LogicalVolume{
			{Name: "pvc-ok", UID: uidOK, VolumeID: uidOK, NodeName: "n", SizeBytes: 10 * gi, CurrentSizeBytes: &small},
			{Name: "pvc-small", UID: uidSm, VolumeID: uidSm, NodeName: "n", SizeBytes: 20 * gi},
			{Name: "pvc-cr", UID: uidCR, VolumeID: uidCR, NodeName: "n", SizeBytes: gi},
			{Name: "elsewhere", UID: "x", VolumeID: "x", NodeName: "other", SizeBytes: gi},
		},
		PVs: []corev1.PersistentVolume{{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-ok"},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "topolvm.io", VolumeHandle: uidOK}},
				ClaimRef:               &corev1.ObjectReference{Namespace: "ns", Name: "data"},
			},
		}},
	}
}

const lvs = `{"report": [{"lv": [
  {"lv_name": "` + uidOK + `", "vg_name": "vg0", "lv_size": "10737418240"},
  {"lv_name": "` + uidSm + `", "vg_name": "vg0", "lv_size": "10737418240B"},
  {"lv_name": "` + uidDk + `", "vg_name": "vg0", "lv_size": "5368709120"},
  {"lv_name": "root", "vg_name": "vg0", "lv_size": "161061273600"},
  {"lv_name": "` + uidOK + `", "vg_name": "other-vg", "lv_size": "1073741824"}
]}]}`

func states(rows []Row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.State+" "+r.LV+" "+r.VG)
	}
	return out
}

func TestLVSDiff(t *testing.T) {
	rows, err := Diff(input(), "n", strings.NewReader(lvs), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"disk-only " + uidDk + " vg0",
		"cr-only " + uidCR + " -",
		"size-mismatch " + uidSm + " vg0",
		"ok " + uidOK + " vg0",
	}
	if got := states(rows); !reflect.DeepEqual(got, want) {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if rows[3].PVC != "ns/data" || rows[2].DiskBytes == nil || *rows[2].DiskBytes != 10*gi || *rows[2].CRBytes != 20*gi {
		t.Errorf("details = %+v / %+v", rows[3], rows[2])
	}
	if !Problems(rows) {
		t.Error("disk-only, cr-only and size-mismatch are problems")
	}
	all, _ := Diff(input(), "n", strings.NewReader(lvs), true)
	if got := states(all); len(got) != 6 || got[4] != "foreign "+uidOK+" other-vg" || got[5] != "foreign root vg0" {
		t.Errorf("with --all: %v", got)
	}
	if _, err := Diff(input(), "n", strings.NewReader("not json"), false); err == nil {
		t.Error("want an error for bad input")
	}
}

func TestCommandKeepsJSONClean(t *testing.T) {
	c := Command("worker-3")
	if strings.Contains(c, " -it ") || !strings.Contains(c, " -i ") || !strings.Contains(c, "--quiet") || !strings.Contains(c, "node/worker-3") {
		t.Errorf("a TTY merges stderr into the JSON; want -i --quiet without -t: %s", c)
	}
}

func TestLVSDiffUsesTheNodesVolumeGroups(t *testing.T) {
	in := input()
	in.Classes = map[string]topolvm.DeviceClass{"ssd": {Name: "ssd", Type: topolvm.TypeThick, Default: true}}
	in.NodeClasses = map[string]map[string]topolvm.DeviceClass{"n": {"ssd": {Name: "ssd", VolumeGroup: "vg0", Type: topolvm.TypeThick, Default: true}}}
	rows, err := Diff(in, "n", strings.NewReader(lvs), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.VG == "other-vg" && r.State != Foreign {
			t.Errorf("other-vg is not a TopoLVM VG on node n: %+v", r)
		}
	}
}

func TestCommandWaitsForAttach(t *testing.T) {
	c := Command("worker-3")
	if !strings.Contains(c, "chroot /host sh -c 'sleep 2; lvs ") || !strings.HasSuffix(c, "' > lvs-worker-3.json") {
		t.Errorf("lvs exits before kubectl debug attaches and its output is lost; pause first: %s", c)
	}
}

func TestLVOfAnotherNodeIsNotALeak(t *testing.T) {
	const uidFar = "55555555-5555-5555-5555-555555555555"
	in := input()
	in.LVs = append(in.LVs, topolvm.LogicalVolume{Name: "pvc-far", UID: uidFar, VolumeID: uidFar, NodeName: "other", SizeBytes: gi})
	body := `{"report": [{"lv": [{"lv_name": "` + uidFar + `", "vg_name": "vg0", "lv_size": "1073741824"}]}]}`
	rows, err := Diff(in, "n", strings.NewReader(body), false)
	if err != nil {
		t.Fatal(err)
	}
	var far *Row
	for i := range rows {
		if rows[i].LV == uidFar {
			far = &rows[i]
		}
	}
	if far == nil || far.State != OtherNode || far.CRNode == nil || *far.CRNode != "other" || !Problems(rows) {
		t.Fatalf("a disk LV the API places on another node is not leaked space: %+v", rows)
	}
	want := []string{
		"lvs shows 1 LV that the API places on other nodes (other); was it captured on n?",
		"no LV on disk matches a LogicalVolume of n; was lvs captured on n?",
	}
	if got := Warnings(rows, "n"); !reflect.DeepEqual(got, want) {
		t.Errorf("warnings = %q", got)
	}
	if got := Warnings([]Row{{State: OK}, {State: DiskOnly}, {State: CROnly}}, "n"); len(got) != 0 {
		t.Errorf("a matching LV means the capture is from n: %q", got)
	}
}
