package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/EsDmitrii/kubectl-topolvm/internal/fixture"
	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

// useFixture makes every command read the named fixture instead of a cluster and records what
// the command asked to collect.
func useFixture(t *testing.T, name string) *kube.CollectOptions {
	t.Helper()
	asked := &kube.CollectOptions{}
	prev := collectFn
	collectFn = func(_ context.Context, _ *globalOptions, req kube.CollectOptions) (report.Input, error) {
		*asked = req
		in, err := fixture.Load(fixture.Path(name))
		if req.PVs == kube.PVSkip {
			in.PVs = nil
		}
		return in, err
	}
	t.Cleanup(func() { collectFn = prev })
	return asked
}

const singleThickPools = `NODE     DEVICE-CLASS    TYPE    SIZE      USED   FREE      USE%   LVS
node-a   ssd (default)   thick   143.5Gi   30Gi   113.5Gi   21%    2
node-b   ssd (default)   thick   60Gi      0      60Gi      0%     0
*        ssd (default)   thick   203.5Gi   30Gi   173.5Gi   15%    2
`

func TestRootRunsPools(t *testing.T) {
	asked := useFixture(t, "single-thick")
	code, out, errOut := runCLI(t)
	if code != exitOK || out != singleThickPools || errOut != "" {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if asked.PVs != kube.PVSkip {
		t.Errorf("pools must not list PVs: %+v", asked)
	}
	if code, out2, _ := runCLI(t, "pools"); code != exitOK || out2 != out {
		t.Errorf("pools subcommand differs from the root command")
	}
}

func jsonKeys(t *testing.T, s string) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("%v in %q", err, s)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestPoolsJSONSectionOnly(t *testing.T) {
	useFixture(t, "single-thick")
	code, out, _ := runCLI(t, "-o", "json")
	want := []string{"breaches", "driver", "generatedAt", "pools", "schemaVersion", "totals", "warnings"}
	if got := jsonKeys(t, out); code != exitOK || !reflect.DeepEqual(got, want) {
		t.Errorf("exit %d keys %v", code, got)
	}
}

func TestVolumesIgnoresKubeconfigNamespace(t *testing.T) {
	asked := useFixture(t, "single-thick")
	kubeconfig := filepath.Join(t.TempDir(), "config")
	cfg := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: ctx, context: {cluster: c, user: u, namespace: ns-b}}]
current-context: ctx
`
	if err := os.WriteFile(kubeconfig, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "volumes", "--kubeconfig", kubeconfig, "--no-headers")
	if lines := strings.Count(out, "\n"); code != exitOK || lines != 2 {
		t.Errorf("without -n: exit %d, %d rows:\n%s", code, lines, out)
	}
	if asked.PVs != kube.PVOptional {
		t.Errorf("volumes must ask for PVs: %+v", asked)
	}
	code, out, _ = runCLI(t, "volumes", "--kubeconfig", kubeconfig, "-n", "ns-a", "--no-headers")
	if code != exitOK || strings.Count(out, "\n") != 1 || !strings.Contains(out, "data-1") {
		t.Errorf("with -n ns-a: exit %d:\n%s", code, out)
	}
}

func TestCheckExitCodes(t *testing.T) {
	asked := useFixture(t, "anomalies")
	code, out, _ := runCLI(t, "check")
	if code != exitFindings || strings.Count(out, "\n") != 12 {
		t.Errorf("anomalies: exit %d:\n%s", code, out)
	}
	if asked.PVs != kube.PVRequired {
		t.Errorf("check must require PVs: %+v", asked)
	}
	useFixture(t, "single-thick")
	code, out, errOut := runCLI(t, "check")
	if code != exitOK || out != "" || errOut != "no findings\n" {
		t.Errorf("clean: exit %d stdout %q stderr %q", code, out, errOut)
	}
	code, out, _ = runCLI(t, "check", "-o", "json")
	if code != exitOK || !strings.Contains(out, `"findings": []`) {
		t.Errorf("clean json: exit %d:\n%s", code, out)
	}
}

func TestCheckSkipAndMinAge(t *testing.T) {
	useFixture(t, "anomalies")
	code, out, _ := runCLI(t, "check", "--skip", "released-pv,lv-error", "--no-headers")
	if code != exitFindings || strings.Count(out, "\n") != 8 {
		t.Errorf("skip: exit %d:\n%s", code, out)
	}
	code, out, _ = runCLI(t, "check", "--min-age", "1m", "--no-headers")
	if code != exitFindings || !strings.Contains(out, "pvc-a2") {
		t.Errorf("--min-age 1m must report the five minute old orphan:\n%s", out)
	}
}

func TestCheckUnknownSkip(t *testing.T) {
	useFixture(t, "anomalies")
	code, _, errOut := runCLI(t, "check", "--skip", "bogus")
	if code != exitError || !strings.Contains(errOut, `unknown check "bogus"`) {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

func TestNoNodes(t *testing.T) {
	prev := collectFn
	collectFn = func(context.Context, *globalOptions, kube.CollectOptions) (report.Input, error) {
		return report.Input{}, kube.ErrNoNodes
	}
	t.Cleanup(func() { collectFn = prev })
	code, _, errOut := runCLI(t)
	if code != exitError || !strings.HasPrefix(errOut, "error: no TopoLVM nodes found") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

func TestWarningsToStderr(t *testing.T) {
	useFixture(t, "ambiguous")
	code, _, errOut := runCLI(t)
	lines := strings.Split(strings.TrimRight(errOut, "\n"), "\n")
	if code != exitOK || len(lines) != 5 {
		t.Fatalf("exit %d stderr:\n%s", code, errOut)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "warning: ") {
			t.Errorf("line %q lacks the warning prefix", l)
		}
	}
	if !strings.Contains(lines[0], "cannot tell the default device class") {
		t.Errorf("first warning = %q", lines[0])
	}
	_, out, errOut := runCLI(t, "-o", "json")
	if errOut != "" || !strings.Contains(out, "cannot tell the default device class") {
		t.Errorf("json must carry warnings in the document only: stderr %q", errOut)
	}
}

func TestFitCLI(t *testing.T) {
	asked := useFixture(t, "fit")
	code, out, errOut := runCLI(t, "fit", "50Gi")
	if code != exitOK || !strings.HasPrefix(out, "NODE ") || !strings.Contains(errOut, "fits on 2 of 3 nodes") {
		t.Errorf("50Gi: exit %d\n%s\n%s", code, out, errOut)
	}
	if asked.Needs&kube.NeedStorage == 0 || asked.Needs&kube.NeedNodeStates == 0 {
		t.Errorf("fit must ask for storage objects and node states: %+v", asked)
	}
	if code, _, _ = runCLI(t, "fit", "50Gi", "--replicas", "3"); code != exitFindings {
		t.Errorf("replicas 3: exit %d", code)
	}
	if code, _, errOut = runCLI(t, "fit", "lots"); code != exitError || !strings.Contains(errOut, `invalid size "lots"`) {
		t.Errorf("bad size: exit %d %q", code, errOut)
	}
	code, out, _ = runCLI(t, "fit", "50Gi", "-o", "json")
	if code != exitOK || !strings.Contains(out, `"nodesThatFit": 2`) || !strings.Contains(out, `"schemaVersion": "v1"`) {
		t.Errorf("json: exit %d\n%s", code, out)
	}
}

func TestHeadroomCLI(t *testing.T) {
	useFixture(t, "fit")
	code, out, _ := runCLI(t, "headroom", "ns-a/data")
	if code != exitOK || !strings.Contains(out, "CAN GROW TO:") || !strings.Contains(out, "110Gi") ||
		!strings.Contains(out, "LAST RESIZE FAILURE:  not enough space") {
		t.Errorf("no size: exit %d\n%s", code, out)
	}
	if code, out, _ = runCLI(t, "headroom", "ns-a/data", "200Gi"); code != exitFindings || !strings.Contains(out, "FITS:") {
		t.Errorf("200Gi: exit %d\n%s", code, out)
	}
	if code, _, _ = runCLI(t, "headroom", "data", "-n", "ns-a", "50Gi"); code != exitOK {
		t.Errorf("-n form: exit %d", code)
	}
	if code, _, errOut := runCLI(t, "headroom", "ns-a/nope"); code != exitError || !strings.Contains(errOut, "not found") {
		t.Errorf("missing pvc: exit %d %q", code, errOut)
	}
}

func TestPendingCLI(t *testing.T) {
	asked := useFixture(t, "pending")
	code, out, _ := runCLI(t, "pending", "--no-headers")
	if code != exitOK || strings.Count(out, "\n") != 4 {
		t.Errorf("exit %d:\n%s", code, out)
	}
	want := kube.NeedPVCs | kube.NeedPods | kube.NeedEvents | kube.NeedStorage
	if asked.Needs&want != want {
		t.Errorf("pending needs = %b", asked.Needs)
	}
	_, out, _ = runCLI(t, "pending", "-o", "wide")
	if !strings.Contains(strings.SplitN(out, "\n", 2)[0], "LAST-EVENT") || !strings.Contains(out, "ResourceExhausted") {
		t.Errorf("wide:\n%s", out)
	}
}

func TestCheckConsistencyCLI(t *testing.T) {
	asked := useFixture(t, "consistency")
	code, out, _ := runCLI(t, "check", "--no-headers")
	if code != exitFindings || strings.Count(out, "\n") != 8 {
		t.Errorf("exit %d:\n%s", code, out)
	}
	want := kube.NeedPVCs | kube.NeedPods | kube.NeedEvents | kube.NeedStorage | kube.NeedTopoLVMPods
	if asked.Needs&want != want {
		t.Errorf("check needs = %b", asked.Needs)
	}
}

func TestNodeCLI(t *testing.T) {
	useFixture(t, "node")
	code, out, _ := runCLI(t, "node", "node-a")
	for _, want := range []string{"READY:", "SINGLE-COPY", "kubectl delete node node-a would make TopoLVM delete 2 PVCs:",
		"  ns-a/data-db-0", "no room", "StatefulSet/db"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if code != exitOK {
		t.Errorf("exit %d", code)
	}
	if c, _, errOut := runCLI(t, "node", "nope"); c != exitError || !strings.Contains(errOut, "is not a TopoLVM node") {
		t.Errorf("unknown node: exit %d %q", c, errOut)
	}
	if code, out, _ = runCLI(t, "node", "node-a", "-o", "json"); code != exitOK || !strings.Contains(out, `"singleCopy": true`) {
		t.Errorf("json: exit %d\n%s", code, out)
	}
}

func TestPoolThresholdsCLI(t *testing.T) {
	useFixture(t, "single-thick")
	code, _, errOut := runCLI(t, "--fail-over", "20")
	if code != exitFindings || !strings.Contains(errOut, "breach: node-a/ssd: 20.9% used > 20% (113.5Gi free)") {
		t.Errorf("fail-over: exit %d %q", code, errOut)
	}
	if code, _, _ = runCLI(t, "pools", "--min-free", "50Gi"); code != exitOK {
		t.Errorf("min-free 50Gi: exit %d", code)
	}
	if code, _, _ = runCLI(t, "--min-free", "100Gi"); code != exitFindings {
		t.Errorf("min-free 100Gi: exit %d", code)
	}
	_, out, _ := runCLI(t, "--fail-over", "20", "-o", "json")
	if !strings.Contains(out, `"breaches": [`) || !strings.Contains(out, "20.9% used > 20%") {
		t.Errorf("json breaches:\n%s", out)
	}
}

func TestSortAndGroupCLI(t *testing.T) {
	useFixture(t, "single-thick")
	if code, _, errOut := runCLI(t, "--sort-by", "colour"); code != exitError || !strings.Contains(errOut, "unknown sort key") {
		t.Errorf("sort key: exit %d %q", code, errOut)
	}
	code, out, _ := runCLI(t, "volumes", "--group-by", "storageclass")
	if code != exitOK || !strings.HasPrefix(out, "STORAGECLASS ") {
		t.Errorf("group-by: exit %d\n%s", code, out)
	}
	if code, _, _ = runCLI(t, "volumes", "--group-by", "colour"); code != exitError {
		t.Errorf("bad group-by: exit %d", code)
	}
	code, out, _ = runCLI(t, "tree", "--node", "node-a")
	if code != exitOK || strings.Contains(out, "node-b") || !strings.Contains(out, "ns-a/data-1") {
		t.Errorf("tree: exit %d\n%s", code, out)
	}
	if code, out, _ = runCLI(t, "--bytes", "--no-headers"); code != exitOK || !strings.Contains(out, "154128089088") {
		t.Errorf("--bytes: %s", out)
	}
	if code, out, _ = runCLI(t, "--color", "always"); code != exitOK || !strings.Contains(out, "\x1b[") {
		t.Logf("no colored cell in single-thick is fine: %q", out)
	}
	if code, _, _ = runCLI(t, "--color", "sometimes"); code != exitError {
		t.Errorf("bad --color: exit %d", code)
	}
}

func TestRBACCLI(t *testing.T) {
	code, out, _ := runCLI(t, "rbac")
	if code != exitOK || !strings.Contains(out, "kind: ClusterRole") || strings.Contains(out, "nodes/proxy") {
		t.Errorf("rbac: exit %d\n%s", code, out)
	}
	if _, out, _ = runCLI(t, "rbac", "--with", "usage", "--topolvm-namespace", "storage"); !strings.Contains(out, "nodes/proxy") || !strings.Contains(out, "namespace: storage") {
		t.Errorf("rbac --with usage:\n%s", out)
	}
	prev := clientsFn
	t.Cleanup(func() { clientsFn = prev })
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		r.Status.Allowed = r.Spec.ResourceAttributes.Resource != "events"
		return true, r, nil
	})
	clientsFn = func(*globalOptions) (*kube.Clients, error) { return &kube.Clients{Core: cs}, nil }
	code, out, _ = runCLI(t, "rbac", "--check")
	if code != exitFindings || !strings.Contains(out, "list   events") || !strings.Contains(out, " no") {
		t.Errorf("rbac --check: exit %d\n%s", code, out)
	}
}

func TestDumpCLI(t *testing.T) {
	asked := useFixture(t, "anomalies")
	dir := filepath.Join(t.TempDir(), "dump")
	code, _, errOut := runCLI(t, "dump", "--dir", dir, "--anonymize")
	if code != exitOK || !strings.Contains(errOut, "wrote "+dir) {
		t.Fatalf("exit %d %q", code, errOut)
	}
	if asked.Needs&kube.NeedMetrics != 0 || asked.Needs&kube.NeedUsage != 0 || asked.Needs&kube.NeedPVCs == 0 {
		t.Errorf("dump needs = %b", asked.Needs)
	}
	if _, err := fixture.Load(dir); err != nil {
		t.Errorf("dump is not a loadable fixture: %v", err)
	}
	if code, _, _ = runCLI(t, "dump"); code != exitError {
		t.Errorf("dump without --dir: exit %d", code)
	}
}

func TestLVSDiffCLI(t *testing.T) {
	useFixture(t, "single-thick")
	lvs := filepath.Join(t.TempDir(), "lvs.json")
	body := `{"report":[{"lv":[{"lv_name":"00000000-0000-0000-0000-000000000001","vg_name":"vg0","lv_size":"10737418240"}]}]}`
	if err := os.WriteFile(lvs, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "lvs-diff", "--node", "node-a", "-f", lvs)
	if code != exitFindings || !strings.Contains(out, "cr-only") || !strings.Contains(out, "ns-a/data-1") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if code, _, errOut := runCLI(t, "lvs-diff", "-f", lvs); code != exitError || !strings.Contains(errOut, "--node") {
		t.Errorf("without --node: exit %d %q", code, errOut)
	}
	if code, out, errOut := runCLI(t, "lvs-diff", "--node", "typo", "-f", lvs); code != exitError ||
		!strings.Contains(errOut, `node "typo" is not a TopoLVM node`) || out != "" {
		t.Errorf("unknown node: exit %d %q %q", code, out, errOut)
	}
}

func TestPendingForbiddenShowsWarnings(t *testing.T) {
	prev := collectFn
	collectFn = func(context.Context, *globalOptions, kube.CollectOptions) (report.Input, error) {
		in, err := fixture.Load(fixture.Path("pending"))
		in.PVCs = nil
		in.Warnings = []string{"cannot list persistentvolumeclaims: forbidden; claim-based output is incomplete"}
		return in, err
	}
	t.Cleanup(func() { collectFn = prev })
	code, _, errOut := runCLI(t, "pending")
	if code != exitError || !strings.Contains(errOut, "warning: cannot list persistentvolumeclaims: forbidden") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

func TestFitHelpMatchesReplicaCounting(t *testing.T) {
	_, out, _ := runCLI(t, "fit", "--help")
	if strings.Contains(out, "Cordoned, NotReady and tainted nodes are\nlisted with a note and do not count") {
		t.Error("fit --help says tainted nodes do not count towards --replicas, but they do")
	}
	if !strings.Contains(out, "Cordoned and NotReady nodes do not count towards --replicas") {
		t.Errorf("fit --help must say which nodes do not count:\n%s", out)
	}
}

func TestStructuredContracts(t *testing.T) {
	useFixture(t, "single-thick")
	lvs := filepath.Join(t.TempDir(), "lvs.json")
	if err := os.WriteFile(lvs, []byte(`{"report":[{"lv":[]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runCLI(t, "lvs-diff", "--node", "node-b", "-f", lvs, "-o", "json")
	if !strings.Contains(out, `"lvs": []`) {
		t.Errorf("empty lvs-diff must print an empty array:\n%s", out)
	}
	prev := clientsFn
	t.Cleanup(func() { clientsFn = prev })
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		r.Status.Allowed = true
		return true, r, nil
	})
	clientsFn = func(*globalOptions) (*kube.Clients, error) { return &kube.Clients{Core: cs}, nil }
	_, out, _ = runCLI(t, "rbac", "--check", "-o", "json")
	if keys := jsonKeys(t, out); !reflect.DeepEqual(keys, []string{"permissions", "schemaVersion", "warnings"}) {
		t.Errorf("rbac --check -o json keys = %v", keys)
	}
}

func TestHelpFollowsCommandName(t *testing.T) {
	root := newRootCmdNamed(newGlobalOptions(), "/usr/local/bin/kubectl-topolvm_capacity")
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, text := range []string{c.Long, c.Example} {
			if strings.Contains(text, "kubectl topolvm ") || strings.HasSuffix(text, "kubectl topolvm") || strings.Contains(text, "kubectl topolvm\n") {
				t.Errorf("%s help still names kubectl topolvm:\n%s", c.Name(), text)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	if !strings.Contains(root.Example, "kubectl topolvm-capacity check") {
		t.Errorf("root example = %q", root.Example)
	}
}

func TestThinNoteOnlyWithPoolSizes(t *testing.T) {
	useFixture(t, "multi-class")
	const note = `warning: device class "nvme-thin" is thin`
	for _, args := range [][]string{{"pools"}, {"tree"}, {"node", "node-a"}} {
		if _, _, errOut := runCLI(t, args...); !strings.Contains(errOut, note) {
			t.Errorf("%v must explain thin sizes: %q", args, errOut)
		}
	}
	for _, args := range [][]string{{"volumes"}, {"check"}, {"pending"}, {"fit", "1Gi"}} {
		if _, _, errOut := runCLI(t, args...); strings.Contains(errOut, note) {
			t.Errorf("%v prints no pool sizes: %q", args, errOut)
		}
	}
}

func TestUnknownFilterValuesFail(t *testing.T) {
	useFixture(t, "single-thick")
	for _, args := range [][]string{
		{"--node", "typo", "--fail-over", "10"},
		{"--device-class", "typo", "--min-free", "1Ti"},
		{"check", "--node", "typo"},
		{"fit", "50Gi", "--device-class", "nope"},
		{"volumes", "--node", "node-a,typo"},
	} {
		code, _, errOut := runCLI(t, args...)
		named := strings.Contains(errOut, `node "typo" is not a TopoLVM node`) ||
			strings.Contains(errOut, `device class "typo" is not on any TopoLVM node`) ||
			strings.Contains(errOut, `device class "nope" is not on any TopoLVM node`)
		if code != exitError || !named {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
	}
	useFixture(t, "anomalies")
	if code, out, _ := runCLI(t, "check", "--node", "node-gone"); code != exitFindings || !strings.Contains(out, "lv-on-missing-node") {
		t.Errorf("a deleted node that still has LogicalVolumes is a valid filter: exit %d\n%s", code, out)
	}
}

func TestTreeJSONPodsAreObjects(t *testing.T) {
	useFixture(t, "node")
	code, out, _ := runCLI(t, "tree", "-o", "json")
	var doc struct {
		Tree struct {
			Pods map[string][]map[string]string `json:"pods"`
		} `json:"tree"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != exitOK {
		t.Fatalf("exit %d, %v:\n%s", code, err, out)
	}
	if got := doc.Tree.Pods["ns-a/data-db-0"]; len(got) != 1 || got[0]["name"] != "db-0" || got[0]["phase"] == "" {
		t.Errorf("pods = %v", doc.Tree.Pods)
	}
}

func TestHeadroomRefusesShrinkAndUnknownRoom(t *testing.T) {
	useFixture(t, "fit")
	if code, _, errOut := runCLI(t, "headroom", "ns-a/data", "5Gi"); code != exitError || !strings.Contains(errOut, "5Gi is below the current 10Gi: a PVC cannot shrink") {
		t.Errorf("shrink: exit %d %q", code, errOut)
	}
	prev := collectFn
	t.Cleanup(func() { collectFn = prev })
	collectFn = func(context.Context, *globalOptions, kube.CollectOptions) (report.Input, error) {
		in, err := fixture.Load(fixture.Path("fit"))
		for i := range in.Nodes {
			if in.Nodes[i].Name == "node-a" {
				in.Nodes[i].Capacity = map[string]int64{}
			}
		}
		return in, err
	}
	if code, out, _ := runCLI(t, "headroom", "ns-a/data", "20Gi"); code != exitFindings || !strings.Contains(out, "<unknown>") {
		t.Errorf("unknown room is not room: exit %d\n%s", code, out)
	}
}

func TestPendingForbiddenNamesTheCauseInJSON(t *testing.T) {
	prev := collectFn
	collectFn = func(context.Context, *globalOptions, kube.CollectOptions) (report.Input, error) {
		in, err := fixture.Load(fixture.Path("pending"))
		in.PVCs = nil
		in.Warnings = []string{"cannot list persistentvolumeclaims: forbidden; claim-based output is incomplete"}
		return in, err
	}
	t.Cleanup(func() { collectFn = prev })
	code, _, errOut := runCLI(t, "pending", "-o", "json")
	if code != exitError || !strings.Contains(errOut, "pending needs persistentvolumeclaims and storageclasses: cannot list persistentvolumeclaims: forbidden") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

func TestThresholdFlagsAreValidated(t *testing.T) {
	useFixture(t, "fit")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"fit", "10Gi", "--replicas", "0"}, "--replicas must be at least 1"},
		{[]string{"fit", "10Gi", "--replicas", "-1"}, "--replicas must be at least 1"},
		{[]string{"--fail-over", "-5"}, "--fail-over must be a percentage from 0 (off) to 100"},
		{[]string{"--fail-over", "150"}, "--fail-over must be a percentage from 0 (off) to 100"},
	} {
		if code, _, errOut := runCLI(t, c.args...); code != exitError || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d %q", c.args, code, errOut)
		}
	}
}

func TestStructuredOutputEverywhere(t *testing.T) {
	if code, _, errOut := runCLI(t, "rbac", "-o", "json"); code != exitError || !strings.Contains(errOut, "rbac prints YAML manifests; -o json and yaml apply to --check") {
		t.Errorf("rbac -o json: exit %d %q", code, errOut)
	}
	code, out, _ := runCLI(t, "version", "-o", "json")
	if code != exitOK || !strings.Contains(out, `"schemaVersion": "v1"`) {
		t.Errorf("version -o json: exit %d\n%s", code, out)
	}
}

func TestNamespaceFlagSaysWhatItFilters(t *testing.T) {
	_, out, _ := runCLI(t, "--help")
	if !strings.Contains(out, "only claims in this namespace (volumes, tree, check, pending); the claim's namespace for headroom") {
		t.Errorf("-n help:\n%s", out)
	}
}

func TestVolumesSortKeyChecks(t *testing.T) {
	useFixture(t, "single-thick")
	if code, _, errOut := runCLI(t, "volumes", "--sort-by", "idle"); code != exitError || !strings.Contains(errOut, "--sort-by idle needs --usage") {
		t.Errorf("idle without usage: exit %d %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, "volumes", "--sort-by", "nope"); code != exitError || !strings.Contains(errOut, "fs-used, idle") {
		t.Errorf("unknown key: exit %d %q", code, errOut)
	}
}

func TestDumpWarnsWithoutAnonymize(t *testing.T) {
	useFixture(t, "anomalies")
	_, _, errOut := runCLI(t, "dump", "--dir", filepath.Join(t.TempDir(), "dump"))
	if !strings.Contains(errOut, "names are not anonymized; pass --anonymize before sharing the dump") {
		t.Errorf("stderr %q", errOut)
	}
	_, out, _ := runCLI(t, "dump", "--help")
	if !strings.Contains(out, "Not renamed:") {
		t.Errorf("help must say what stays:\n%s", out)
	}
}
