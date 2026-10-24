package dump

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/fixture"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

func findings(in report.Input) []string {
	r := report.Build(in, report.Options{MinAge: 15 * time.Minute, Checks: true})
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Check+" "+f.Object+" "+f.Detail)
	}
	return out
}

func checks(in report.Input) []string {
	r := report.Build(in, report.Options{MinAge: 15 * time.Minute, Checks: true})
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Check)
	}
	return out
}

func roundTrip(t *testing.T, name string, anonymize bool) (report.Input, report.Input, string) {
	t.Helper()
	in, err := fixture.Load(fixture.Path(name))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "out")
	if werr := Write(dir, in, anonymize); werr != nil {
		t.Fatal(werr)
	}
	back, err := fixture.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return in, back, dir
}

func TestDumpRoundTrip(t *testing.T) {
	for _, name := range []string{"anomalies", "consistency", "multi-class", "legacy", "fit", "node"} {
		in, back, _ := roundTrip(t, name, false)
		if got, want := findings(back), findings(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: findings after dump:\n%s\nwant:\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		a, b := report.Build(in, report.Options{}), report.Build(back, report.Options{})
		if !reflect.DeepEqual(a.Pools, b.Pools) || len(a.Volumes) != len(b.Volumes) {
			t.Errorf("%s: pools or volumes changed after dump", name)
		}
	}
}

func TestDumpAnonymize(t *testing.T) {
	in, back, dir := roundTrip(t, "node", true)
	if got, want := checks(back), checks(in); !reflect.DeepEqual(got, want) {
		t.Errorf("checks after anonymizing: %v, want %v", got, want)
	}
	var all strings.Builder
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		all.Write(data)
	}
	for _, leak := range []string{"node-a", "node-b", `"ns-a"`, "data-db-0", "data-ha-1", `"db-0"`, `"ha-0"`, `"name": "db"`} {
		if strings.Contains(all.String(), leak) {
			t.Errorf("anonymized dump still contains %s", leak)
		}
	}
}

func TestDumpAnonymizesEventMessages(t *testing.T) {
	in, err := fixture.Load(fixture.Path("node"))
	if err != nil {
		t.Fatal(err)
	}
	in.Events = []corev1.Event{{
		ObjectMeta:     metav1.ObjectMeta{Name: "e", Namespace: "ns-a"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "ns-a", Name: "db-0"},
		Reason:         "FailedScheduling",
		Message:        "pod ns-a/db-0 cannot go to node-a: 0/3 nodes are available",
	}}
	dir := filepath.Join(t.TempDir(), "out")
	if werr := Write(dir, in, true); werr != nil {
		t.Fatal(werr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(data); strings.Contains(s, "node-a") || strings.Contains(s, "db-0") || strings.Contains(s, "ns-a") ||
		!strings.Contains(s, "0/3 nodes are available") {
		t.Errorf("events:\n%s", s)
	}
}

func TestDumpStripsPodSpecs(t *testing.T) {
	in, err := fixture.Load(fixture.Path("node"))
	if err != nil {
		t.Fatal(err)
	}
	in.Pods[0].Spec.Containers[0].Env = []corev1.EnvVar{{Name: "PASSWORD", Value: "hunter2"}}
	in.Pods[0].Spec.Containers[0].Image = "registry.example.com/secret/app:1"
	dir := filepath.Join(t.TempDir(), "out")
	if err := Write(dir, in, false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "pods.json"))
	if strings.Contains(string(data), "hunter2") || strings.Contains(string(data), "registry.example.com") {
		t.Errorf("pod env or image leaked:\n%s", data)
	}
}

func TestDumpRefusesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, report.Input{}, false); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("err = %v", err)
	}
}

func TestDumpKeepsOnlyTopoLVMObjects(t *testing.T) {
	in, err := fixture.Load(fixture.Path("pending"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "out")
	if werr := Write(dir, in, true); werr != nil {
		t.Fatal(werr)
	}
	back, err := fixture.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.StorageClasses) != 2 || len(back.PVCs) != 5 || len(back.Pods) != 2 {
		t.Errorf("kept %d storageclasses, %d claims, %d pods", len(back.StorageClasses), len(back.PVCs), len(back.Pods))
	}
	var all strings.Builder
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		all.Write(data)
	}
	for _, leak := range []string{"local-path", "rancher.io", "topolvm-wffc", "app-local", `"name": "app"`, "node-3"} {
		if strings.Contains(all.String(), leak) {
			t.Errorf("dump still contains %s", leak)
		}
	}
	if got, want := checks(back), checks(in); !reflect.DeepEqual(got, want) {
		t.Errorf("checks after filtering: %v, want %v", got, want)
	}
}

func TestDumpDropsForeignCSINodes(t *testing.T) {
	in, err := fixture.Load(fixture.Path("consistency"))
	if err != nil {
		t.Fatal(err)
	}
	foreign := in.CSINodes[0].DeepCopy()
	foreign.Name = "gpu-box"
	foreign.Spec.Drivers[0].Name = "csi.example.com"
	in.CSINodes = append(in.CSINodes, *foreign)
	dir := filepath.Join(t.TempDir(), "out")
	if werr := Write(dir, in, true); werr != nil {
		t.Fatal(werr)
	}
	back, err := fixture.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.CSINodes) != 3 {
		t.Errorf("csinodes = %d, want the 3 TopoLVM ones", len(back.CSINodes))
	}
}

func TestDumpLeaksNothingThroughForeignObjects(t *testing.T) {
	in, err := fixture.Load(fixture.Path("consistency"))
	if err != nil {
		t.Fatal(err)
	}
	in.CSINodes[0].Spec.Drivers = append(in.CSINodes[0].Spec.Drivers, storagev1.CSINodeDriver{
		Name: "pd.csi.storage.gke.io", NodeID: "projects/acme-prod/zones/europe-west1-b/instances/real-node-name",
	})
	foreign := in.Capacities[0].DeepCopy()
	foreign.Namespace, foreign.StorageClassName = "acme-storage", "acme-fast"
	in.Capacities = append(in.Capacities, *foreign)
	in.NodeStates = map[string]report.NodeState{"node-a": {Ready: true, Taints: []corev1.Taint{
		{Key: "team", Value: "acme-payments", Effect: corev1.TaintEffectNoSchedule}}}}
	dir := filepath.Join(t.TempDir(), "out")
	if werr := Write(dir, in, true); werr != nil {
		t.Fatal(werr)
	}
	var all strings.Builder
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		all.Write(data)
	}
	for _, leak := range []string{"acme", "real-node-name", "gke"} {
		if strings.Contains(all.String(), leak) {
			t.Errorf("dump still contains %q", leak)
		}
	}
	back, err := fixture.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Capacities) != 1 || len(back.NodeStates) != 1 {
		t.Errorf("capacities %d node states %d", len(back.Capacities), len(back.NodeStates))
	}
	for _, s := range back.NodeStates {
		if len(s.Taints) != 1 || s.Taints[0].Key != "taint-1" || s.Taints[0].Effect != corev1.TaintEffectNoSchedule {
			t.Errorf("a site's own taint key is renamed, its effect survives: %+v", s.Taints)
		}
	}
}

func TestDumpKeepsPhaseTransitionTime(t *testing.T) {
	in, err := fixture.Load(fixture.Path("anomalies"))
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	for i := range in.PVs {
		if in.PVs[i].Status.Phase == corev1.VolumeReleased {
			in.PVs[i].Status.LastPhaseTransitionTime = new(metav1.NewTime(in.Now.Add(-time.Minute)))
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("anomalies has no Released PV")
	}
	dir := filepath.Join(t.TempDir(), "out")
	if werr := Write(dir, in, false); werr != nil {
		t.Fatal(werr)
	}
	back, err := fixture.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := findings(back), findings(in); !reflect.DeepEqual(got, want) {
		t.Errorf("findings after dump:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDumpKeepsPVDeleteFailures(t *testing.T) {
	in, err := fixture.Load(fixture.Path("anomalies"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range in.PVs {
		if in.PVs[i].Name == "pvc-a5" {
			in.PVs[i].Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		}
	}
	in.Events = append(in.Events, corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolume", Name: "pvc-a5"},
		Reason:         "VolumeFailedDelete", Message: "rpc error: code = DeadlineExceeded",
		LastTimestamp: metav1.NewTime(in.Now.Add(-time.Minute)),
	})
	for _, anonymize := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "out")
		if err := Write(dir, in, anonymize); err != nil {
			t.Fatal(err)
		}
		back, err := fixture.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range findings(back) {
			found = found || strings.HasSuffix(f, "; last error: rpc error: code = DeadlineExceeded")
		}
		if !found {
			t.Errorf("anonymize=%v: the delete failure is lost:\n%s", anonymize, strings.Join(findings(back), "\n"))
		}
	}
}

func TestAnonymizerScalesWithMessageLength(t *testing.T) {
	a := newAnonymizer(true)
	for i := range 20000 {
		a.note("node", fmt.Sprintf("worker-%d.dc1.example.com", i))
	}
	a.freeze()
	msg := strings.Repeat("0/20000 nodes are available: worker-17.dc1.example.com had volume node affinity conflict. ", 3)
	start := time.Now()
	var out string
	for range 5000 {
		out = a.message(msg)
	}
	if strings.Contains(out, "worker-17") || !strings.Contains(out, "node-") {
		t.Errorf("message = %q", out)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("5000 messages against 20000 names took %s", d)
	}
}

// originalNames are the names --anonymize promises to replace.
func originalNames(in report.Input) map[string]bool {
	names := map[string]bool{}
	add := func(n string) {
		if n != "" {
			names[n] = true
		}
	}
	for _, n := range in.Nodes {
		add(n.Name)
	}
	for n := range in.AbsentNodes {
		add(n)
	}
	for _, c := range in.PVCs {
		add(c.Namespace)
		add(c.Name)
	}
	for _, p := range in.Pods {
		add(p.Namespace)
		add(p.Name)
		for _, o := range p.OwnerReferences {
			add(o.Name)
		}
	}
	for _, sc := range in.StorageClasses {
		add(sc.Name)
	}
	for _, pv := range in.PVs {
		if pv.Spec.ClaimRef != nil {
			add(pv.Spec.ClaimRef.Namespace)
			add(pv.Spec.ClaimRef.Name)
		}
	}
	return names
}

// stringValues walks a decoded JSON document and returns every string value, never a key.
func stringValues(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			stringValues(e, out)
		}
	case map[string]any:
		for _, e := range t {
			stringValues(e, out)
		}
	}
}

func TestAnonymizedDumpHasNoOriginalNames(t *testing.T) {
	for _, name := range []string{"anomalies", "node", "pending", "consistency", "fit", "extended"} {
		in, err := fixture.Load(fixture.Path(name))
		if err != nil {
			t.Fatal(err)
		}
		if len(in.LVs) > 0 {
			in.LVs[0].Code, in.LVs[0].Message = 13, "lvcreate failed on "+in.LVs[0].NodeName
		}
		dir := filepath.Join(t.TempDir(), "out")
		if err := Write(dir, in, true); err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		var values []string
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var doc any
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			stringValues(doc, &values)
		}
		for n := range originalNames(in) {
			word := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(n) + `($|[^A-Za-z0-9_-])`)
			for _, v := range values {
				if word.MatchString(v) {
					t.Errorf("%s: %q survives anonymizing in %q", name, n, v)
					break
				}
			}
		}
	}
}

func TestAnonymizeFinalizersTaintsAndAddresses(t *testing.T) {
	in, err := fixture.Load(fixture.Path("anomalies"))
	if err != nil {
		t.Fatal(err)
	}
	in.LVs[0].Finalizers = []string{"topolvm.io/logicalvolume", "acme.example.com/backup-hold", "kubernetes.io/pv-protection"}
	in.LVs[0].Code, in.LVs[0].Message = 13, "lvmd at 10.20.30.40:9100 refused"
	in.NodeStates = map[string]report.NodeState{in.Nodes[0].Name: {Ready: true, Taints: []corev1.Taint{
		{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule},
		{Key: "acme.example.com/team-payments", Value: "x", Effect: corev1.TaintEffectNoSchedule},
	}}}
	dir := filepath.Join(t.TempDir(), "out")
	if werr := Write(dir, in, true); werr != nil {
		t.Fatal(werr)
	}
	var all strings.Builder
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		body, _ := os.ReadFile(f)
		all.Write(body)
	}
	for _, leak := range []string{"acme.example.com", "10.20.30.40"} {
		if strings.Contains(all.String(), leak) {
			t.Errorf("%s survives anonymizing", leak)
		}
	}
	for _, kept := range []string{"topolvm.io/logicalvolume", "kubernetes.io/pv-protection", "node.kubernetes.io/unschedulable", "finalizer-1", "taint-1"} {
		if !strings.Contains(all.String(), kept) {
			t.Errorf("%s is missing", kept)
		}
	}
}
