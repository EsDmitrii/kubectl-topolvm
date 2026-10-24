package output

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/EsDmitrii/kubectl-topolvm/internal/fixture"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRenderMatchesTabwriter(t *testing.T) {
	rows := [][]string{
		{"NODE", "DEVICE-CLASS", "TYPE", "SIZE", "USED", "FREE", "USE%", "LVS"},
		{"node-a", "ssd (default)", "thick", "143.5Gi", "30Gi", "113.5Gi", "21%", "2"},
		{"node-b", "ssd (default)", "thick", "60Gi", "0", "60Gi", "0%", "0"},
		{"*", "ssd (default)", "thick", "203.5Gi", "30Gi", "173.5Gi", "15%", "2"},
	}
	var want bytes.Buffer
	tw := tabwriter.NewWriter(&want, 0, 4, 3, ' ', 0)
	for _, r := range rows {
		_, _ = fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	tbl := newTable()
	for _, r := range rows {
		tbl.row(r...)
	}
	var got bytes.Buffer
	if err := tbl.render(&got); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Errorf("render:\n%s\ntabwriter:\n%s", got.String(), want.String())
	}
}

func loadReport(t *testing.T, name string) report.Report {
	t.Helper()
	in, err := fixture.Load(fixture.Path(name))
	if err != nil {
		t.Fatal(err)
	}
	return report.Build(in, report.Options{MinAge: 15 * time.Minute, Checks: true, SizeNotes: true})
}

const singleThickPools = `NODE     DEVICE-CLASS    TYPE    SIZE      USED   FREE      USE%   LVS
node-a   ssd (default)   thick   143.5Gi   30Gi   113.5Gi   21%    2
node-b   ssd (default)   thick   60Gi      0      60Gi      0%     0
*        ssd (default)   thick   203.5Gi   30Gi   173.5Gi   15%    2
`

func TestPoolsSingleThick(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePools(&buf, loadReport(t, "single-thick"), TableOptions{}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != singleThickPools {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), singleThickPools)
	}
}

func TestPoolsNoHeaders(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePools(&buf, loadReport(t, "single-thick"), TableOptions{NoHeaders: true}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "node-a") || !strings.HasPrefix(lines[1], "node-b") {
		t.Errorf("no-headers output:\n%s", buf.String())
	}
}

func topKeys(t *testing.T, data []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestStructuredPoolsKeys(t *testing.T) {
	var buf bytes.Buffer
	if err := Structured(&buf, loadReport(t, "single-thick"), SectionPools, "json"); err != nil {
		t.Fatal(err)
	}
	want := []string{"breaches", "driver", "generatedAt", "pools", "schemaVersion", "totals", "warnings"}
	if got := topKeys(t, buf.Bytes()); !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	if !strings.Contains(buf.String(), `"warnings": []`) {
		t.Errorf("warnings must be an empty array:\n%s", buf.String())
	}
}

func TestStructuredYAML(t *testing.T) {
	var buf bytes.Buffer
	if err := Structured(&buf, loadReport(t, "single-thick"), SectionPools, "yaml"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "breaches: []\ndriver: topolvm.io\n") || !strings.Contains(buf.String(), "schemaVersion: v1") {
		t.Errorf("yaml:\n%s", buf.String())
	}
}

// golden renders one output per file for every fixture and compares it with want/<file>.
type golden struct {
	file   string
	render func(*bytes.Buffer, report.Report) error
}

var goldens = []golden{
	{"pools.txt", func(b *bytes.Buffer, r report.Report) error { return WritePools(b, r, TableOptions{}) }},
	{"pools-wide.txt", func(b *bytes.Buffer, r report.Report) error { return WritePools(b, r, TableOptions{Wide: true}) }},
	{"pools.json", func(b *bytes.Buffer, r report.Report) error { return Structured(b, r, SectionPools, "json") }},
}

func TestGolden(t *testing.T) {
	dirs, err := filepath.Glob(fixture.Path("*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
			continue
		}
		name := filepath.Base(dir)
		r := loadReport(t, name)
		for _, g := range goldens {
			t.Run(name+"/"+g.file, func(t *testing.T) {
				var buf bytes.Buffer
				if err := g.render(&buf, r); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "want", g.file)
				if *update {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%v (run make golden)", err)
				}
				if buf.String() != string(want) {
					t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", path, buf.String(), want)
				}
			})
		}
	}
}

const singleThickVolumes = `NODE     DEVICE-CLASS   SIZE   NAMESPACE   PVC      STORAGECLASS      STATUS   AGE
node-a   ssd            10Gi   ns-a        data-1   topolvm-retain    Bound    23d
node-a   ssd            20Gi   ns-b        data-2   topolvm-default   Bound    4d12h
`

func TestVolumesSingleThick(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteVolumes(&buf, loadReport(t, "single-thick"), TableOptions{}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != singleThickVolumes {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), singleThickVolumes)
	}
}

func TestVolumesFutureCreation(t *testing.T) {
	r := loadReport(t, "single-thick")
	r.Volumes[0].CreatedAt = r.GeneratedAt.Add(10 * time.Minute)
	var buf bytes.Buffer
	if err := WriteVolumes(&buf, r, TableOptions{NoHeaders: true}); err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(buf.String(), "\n"); !strings.HasSuffix(first, " 0s") {
		t.Errorf("future creation must show 0s: %q", first)
	}
}

func init() {
	goldens = append(goldens,
		golden{"volumes.txt", func(b *bytes.Buffer, r report.Report) error { return WriteVolumes(b, r, TableOptions{}) }},
		golden{"volumes-wide.txt", func(b *bytes.Buffer, r report.Report) error { return WriteVolumes(b, r, TableOptions{Wide: true}) }},
		golden{"volumes.json", func(b *bytes.Buffer, r report.Report) error { return Structured(b, r, SectionVolumes, "json") }},
	)
}

func TestFindingsTable(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFindings(&buf, loadReport(t, "anomalies"), TableOptions{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 12 || !strings.HasPrefix(lines[0], "CHECK ") || !strings.Contains(lines[0], "DETAIL") {
		t.Fatalf("findings table:\n%s", buf.String())
	}
	if !strings.HasPrefix(lines[2], "pv-without-lv ") || !strings.Contains(lines[2], "<none>") {
		t.Errorf("pv-without-lv row must show <none> size: %q", lines[2])
	}
}

func init() {
	goldens = append(goldens,
		golden{"check.txt", func(b *bytes.Buffer, r report.Report) error { return WriteFindings(b, r, TableOptions{}) }},
		golden{"check.json", func(b *bytes.Buffer, r report.Report) error { return Structured(b, r, SectionCheck, "json") }},
	)
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestColorPaddingIgnoresANSI(t *testing.T) {
	r := loadReport(t, "single-thick")
	var plain, colored bytes.Buffer
	if err := WritePools(&plain, r, TableOptions{}); err != nil {
		t.Fatal(err)
	}
	r.Pools[0].UsedBytes = 120 * (1 << 30) // 120 of ~234Gi on node-a is above 50%; push it over 85%
	r.Pools[0].SizeBytes = new(int64(130 * (1 << 30)))
	if err := WritePools(&colored, r, TableOptions{Color: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(colored.String(), "\x1b[31m92%\x1b[0m") {
		t.Errorf("USE%% over 85 must be red:\n%q", colored.String())
	}
	stripped := ansi.ReplaceAllString(colored.String(), "")
	var recolored bytes.Buffer
	if err := WritePools(&recolored, r, TableOptions{}); err != nil {
		t.Fatal(err)
	}
	if stripped != recolored.String() {
		t.Errorf("colors changed the layout:\n%s\nvs\n%s", stripped, recolored.String())
	}
}

func TestBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePools(&buf, loadReport(t, "single-thick"), TableOptions{Bytes: true, NoHeaders: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "154128089088") || !strings.Contains(buf.String(), "121915834368") {
		t.Errorf("--bytes:\n%s", buf.String())
	}
}

func TestSortPools(t *testing.T) {
	r := loadReport(t, "single-thick")
	if err := SortPools(&r, "use%"); err != nil {
		t.Fatal(err)
	}
	if r.Pools[0].Node != "node-a" {
		t.Errorf("use%% sorts the fullest first: %v", r.Pools[0].Node)
	}
	if err := SortPools(&r, "free"); err != nil || r.Pools[0].Node != "node-a" {
		t.Errorf("free: %v %v", r.Pools[0].Node, err)
	}
	if err := SortPools(&r, "node"); err != nil || r.Pools[0].Node != "node-a" {
		t.Errorf("node: %v", err)
	}
	err := SortPools(&r, "colour")
	if err == nil || err.Error() != `unknown sort key "colour"; want one of node, device-class, size, used, free, use%, lvs` {
		t.Errorf("err = %v", err)
	}
}

func TestSortVolumes(t *testing.T) {
	r := loadReport(t, "single-thick")
	if err := SortVolumes(&r, "size"); err != nil || *r.Volumes[0].PVC != "data-2" {
		t.Errorf("size: %v", err)
	}
	if err := SortVolumes(&r, "age"); err != nil || *r.Volumes[0].PVC != "data-1" {
		t.Errorf("age sorts the oldest first: %v", err)
	}
	if err := SortVolumes(&r, "bogus"); err == nil || !strings.Contains(err.Error(), "node, namespace, pvc, size, age, status") {
		t.Errorf("err = %v", err)
	}
}

func TestGroupByNamespace(t *testing.T) {
	r := loadReport(t, "multi-class")
	var buf bytes.Buffer
	if err := WriteGroups(&buf, report.GroupVolumes(r.Volumes, "namespace"), "namespace", TableOptions{}); err != nil {
		t.Fatal(err)
	}
	want := "NAMESPACE   VOLUMES   SIZE\nns-b        2         200Gi\n<none>      1         100Gi\nns-a        2         15Gi\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestTreeASCII(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTree(&buf, loadReport(t, "single-thick"), nil, TableOptions{ASCII: true}); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(buf.String(), "├└│") || !strings.Contains(buf.String(), `\- ssd (default)`) {
		t.Errorf("ascii tree:\n%s", buf.String())
	}
}

func init() {
	goldens = append(goldens,
		golden{"tree.txt", func(b *bytes.Buffer, r report.Report) error { return WriteTree(b, r, nil, TableOptions{}) }})
}

func TestMetricsColumnsWithoutWide(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePools(&buf, loadReport(t, "single-thick"), TableOptions{Metrics: true}); err != nil {
		t.Fatal(err)
	}
	if header, _, _ := strings.Cut(buf.String(), "\n"); !strings.Contains(header, "VG-SIZE") || !strings.Contains(header, "OTHER") {
		t.Errorf("--metrics must show its columns without -o wide: %q", header)
	}
}

func TestWriteNodeDeletion(t *testing.T) {
	for _, c := range []struct {
		nr   report.NodeReport
		want string
	}{
		{report.NodeReport{LogicalVolumesDeleted: 3},
			"kubectl delete node n would make TopoLVM delete its 3 LogicalVolumes and claims that are unknown: persistentvolumeclaims or storageclasses were not collected\n"},
		{report.NodeReport{DeletedOnNodeDelete: []string{}, ClaimsLeftWithoutLV: []string{}},
			"kubectl delete node n would not make TopoLVM delete any PVC\n"},
		{report.NodeReport{DeletedOnNodeDelete: []string{"a/x"}, LogicalVolumesDeleted: 2, ClaimsLeftWithoutLV: []string{"a/y"}},
			"kubectl delete node n would make TopoLVM delete 1 PVC:\n  a/x\nand its 2 LogicalVolumes; these volumes would keep a PV without an LV:\n  a/y\n"},
		{report.NodeReport{DeletedOnNodeDelete: []string{"a/x", "a/y"}, LogicalVolumesDeleted: 1, ClaimsLeftWithoutLV: []string{}},
			"kubectl delete node n would make TopoLVM delete 2 PVCs:\n  a/x\n  a/y\nand its 1 LogicalVolume\n"},
		{report.NodeReport{State: &report.NodeState{}, DeletedOnNodeDelete: []string{}, ClaimsLeftWithoutLV: []string{}},
			"kubectl delete node n would make TopoLVM delete nothing: the Node has no topolvm.io/node finalizer\n"},
	} {
		var buf bytes.Buffer
		c.nr.Node, c.nr.Volumes, c.nr.Evacuation = "n", []report.NodeVolume{}, []report.Placement{}
		if err := WriteNode(&buf, c.nr, "topolvm.io", TableOptions{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), "\n"+c.want+"\n") {
			t.Errorf("want %q in:\n%s", c.want, buf.String())
		}
	}
}

func TestMetricsColumnsTellUnknownFromNotApplicable(t *testing.T) {
	vg := int64(100 << 30)
	r := report.Report{Pools: []report.Pool{
		{Node: "n", DeviceClass: "ssd", Type: "thick", VGSizeBytes: &vg},
		{Node: "n", DeviceClass: "thin", Type: "thin", VGSizeBytes: &vg},
	}}
	var buf bytes.Buffer
	if err := WritePools(&buf, r, TableOptions{Metrics: true, NoHeaders: true}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	thick, thin := strings.Fields(lines[0]), strings.Fields(lines[1])
	if got := thick[len(thick)-3:]; !reflect.DeepEqual(got, []string{"<unknown>", "-", "-"}) {
		t.Errorf("thick OTHER is unknown, DATA%% and META%% do not apply: %v", got)
	}
	if got := thin[len(thin)-3:]; !reflect.DeepEqual(got, []string{"-", "<unknown>", "<unknown>"}) {
		t.Errorf("thin OTHER does not apply, DATA%% and META%% are unknown: %v", got)
	}
}
