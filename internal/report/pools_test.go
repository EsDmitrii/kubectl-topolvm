package report_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/EsDmitrii/kubectl-topolvm/internal/fixture"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

const gi = int64(1) << 30

func load(t *testing.T, name string) report.Input {
	t.Helper()
	in, err := fixture.Load(fixture.Path(name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return in
}

func build(in report.Input) report.Report {
	return report.Build(in, report.Options{MinAge: 15 * time.Minute, Checks: true, SizeNotes: true})
}

func i64(p *int64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

func f64(p *float64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

func classes(pools []report.Pool) []string {
	out := make([]string, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.Node+"/"+p.DeviceClass)
	}
	return out
}

func TestBuildPoolsSingleThick(t *testing.T) {
	r := build(load(t, "single-thick"))
	if r.SchemaVersion != "v1" || r.Driver != "topolvm.io" || !r.GeneratedAt.Equal(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("header = %q %q %v", r.SchemaVersion, r.Driver, r.GeneratedAt)
	}
	if len(r.Pools) != 2 {
		t.Fatalf("pools = %v", classes(r.Pools))
	}
	a, b := r.Pools[0], r.Pools[1]
	if a.Node != "node-a" || a.DeviceClass != "ssd" || !a.Default || a.Type != "thick" ||
		a.VolumeGroup == nil || *a.VolumeGroup != "vg0" || i64(a.SpareBytes) != "10737418240" ||
		i64(a.SizeBytes) != "154128089088" || a.UsedBytes != 32212254720 || i64(a.FreeBytes) != "121915834368" ||
		f64(a.UsedPercent) != "20.9" || a.Volumes != 2 {
		t.Errorf("node-a = %+v size=%s free=%s pct=%s", a, i64(a.SizeBytes), i64(a.FreeBytes), f64(a.UsedPercent))
	}
	if b.Node != "node-b" || i64(b.SizeBytes) != "64424509440" || b.UsedBytes != 0 || f64(b.UsedPercent) != "0" || b.Volumes != 0 {
		t.Errorf("node-b = %+v", b)
	}
	if len(r.Totals) != 1 {
		t.Fatalf("totals = %+v", r.Totals)
	}
	tot := r.Totals[0]
	if tot.DeviceClass != "ssd" || !tot.Default || tot.Nodes != 2 || i64(tot.SizeBytes) != "218552598528" ||
		tot.UsedBytes != 32212254720 || i64(tot.FreeBytes) != "186340343808" || i64(tot.MaxFreeBytes) != "121915834368" ||
		tot.MaxFreeNode != "node-a" || f64(tot.UsedPercent) != "14.7" || tot.Volumes != 2 {
		t.Errorf("total = %+v", tot)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings = %v", r.Warnings)
	}
}

func TestBuildPoolsNoDoubleCount(t *testing.T) {
	r := build(load(t, "multi-class"))
	if got := classes(r.Pools); !reflect.DeepEqual(got, []string{"node-a/dc1", "node-a/dc2", "node-a/nvme-thin"}) {
		t.Fatalf("pools = %v", got)
	}
	if r.Pools[0].UsedBytes != 10*gi || !r.Pools[0].Default {
		t.Errorf("dc1 = %+v", r.Pools[0])
	}
	thin := r.Pools[2]
	if thin.Type != "thin" || thin.UsedPercent != nil || thin.UsedBytes != 322122547200 {
		t.Errorf("nvme-thin = %+v", thin)
	}
	want := []string{`device class "nvme-thin" is thin: SIZE, USED and FREE are virtual (overprovisioned) sizes`}
	if !reflect.DeepEqual(r.Warnings, want) {
		t.Errorf("warnings = %q", r.Warnings)
	}
	if len(r.Totals) != 0 {
		t.Errorf("single node must not have totals: %+v", r.Totals)
	}
}

func TestBuildPoolsNoConfigMap(t *testing.T) {
	in := load(t, "no-configmap")
	r := build(in)
	p := r.Pools[0]
	if !p.Default || p.Type != "unknown" || p.VolumeGroup != nil || p.SpareBytes != nil {
		t.Errorf("pool = %+v", p)
	}
	w1 := []string{`type of device class "ssd" is unknown (no readable lvmd config); USE% assumes thick, pass --thin=ssd if it is a thin pool`}
	if !reflect.DeepEqual(r.Warnings, w1) {
		t.Errorf("warnings = %q", r.Warnings)
	}

	in.Thin = []string{"ssd"}
	r = build(in)
	if r.Pools[0].Type != "thin" || r.Pools[0].UsedPercent != nil {
		t.Errorf("--thin pool = %+v", r.Pools[0])
	}
	w5 := []string{`device class "ssd" is thin: SIZE, USED and FREE are virtual (overprovisioned) sizes`}
	if !reflect.DeepEqual(r.Warnings, w5) {
		t.Errorf("--thin warnings = %q", r.Warnings)
	}
}

func TestBuildPoolsThinFlagOverridesConfigMap(t *testing.T) {
	in := load(t, "single-thick")
	in.Thin = []string{"ssd"}
	if p := build(in).Pools[0]; p.Type != "thin" {
		t.Errorf("--thin must override the lvmd config: %+v", p)
	}
}

func TestBuildPoolsThinHeuristic(t *testing.T) {
	in := load(t, "no-configmap")
	in.LVs = append(in.LVs, topolvm.LogicalVolume{Name: "snap", UID: "s", NodeName: "node-a", SizeBytes: gi, Source: "pvc-1", AccessType: "ro"})
	if p := build(in).Pools[0]; p.Type != "thin" {
		t.Errorf("an LV with spec.source must mark the class thin: %+v", p)
	}
}

func TestBuildPoolsAmbiguous(t *testing.T) {
	r := build(load(t, "ambiguous"))
	if got := classes(r.Pools); !reflect.DeepEqual(got, []string{"node-a/<default>", "node-a/dc1", "node-a/dc2", "node-b/"}) {
		t.Fatalf("pools = %v", got)
	}
	d := r.Pools[0]
	if d.FreeBytes != nil || d.SizeBytes != nil || d.UsedBytes != gi || d.Type != "unknown" || d.Default {
		t.Errorf("<default> = %+v", d)
	}
	if n := r.Pools[3]; n.FreeBytes != nil || n.Type != "unknown" || n.Volumes != 0 {
		t.Errorf("node-b = %+v", n)
	}
	want := []string{
		"node node-a: cannot tell the default device class; volumes without deviceClass are shown as <default>",
		"node node-a: ignoring unparsable annotation capacity.topolvm.io/broken",
		"node node-b has no capacity.topolvm.io/* annotations: is topolvm-node running there?",
		`type of device class "dc1" is unknown (no readable lvmd config); USE% assumes thick, pass --thin=dc1 if it is a thin pool`,
		`type of device class "dc2" is unknown (no readable lvmd config); USE% assumes thick, pass --thin=dc2 if it is a thin pool`,
	}
	if !reflect.DeepEqual(r.Warnings, want) {
		t.Errorf("warnings:\n%q\nwant\n%q", r.Warnings, want)
	}
}

func TestBuildPoolsLegacy(t *testing.T) {
	r := build(load(t, "legacy"))
	if r.Driver != "topolvm.cybozu.com" || len(r.Pools) != 1 {
		t.Fatalf("report = %+v", r)
	}
	p := r.Pools[0]
	if p.DeviceClass != "ssd" || i64(p.FreeBytes) != "53687091200" || p.UsedBytes != 8*gi || p.Volumes != 1 {
		t.Errorf("pool = %+v", p)
	}
}

func TestBuildPoolsFilters(t *testing.T) {
	r := report.Build(load(t, "single-thick"), report.Options{Nodes: []string{"node-b"}})
	if got := classes(r.Pools); !reflect.DeepEqual(got, []string{"node-b/ssd"}) || len(r.Totals) != 0 {
		t.Errorf("pools = %v, totals = %+v", got, r.Totals)
	}
	r = report.Build(load(t, "multi-class"), report.Options{DeviceClasses: []string{"dc2"}})
	if got := classes(r.Pools); !reflect.DeepEqual(got, []string{"node-a/dc2"}) || len(r.Warnings) != 0 {
		t.Errorf("pools = %v, warnings = %q", got, r.Warnings)
	}
}

func TestBuildPoolsCurrentSize(t *testing.T) {
	cur := int64(20484) << 20
	in := report.Input{
		Driver: topolvm.Current,
		Nodes:  []topolvm.Node{{Name: "n", Capacity: map[string]int64{"00default": gi, "ssd": gi}}},
		LVs:    []topolvm.LogicalVolume{{Name: "a", UID: "a", VolumeID: "a", NodeName: "n", SizeBytes: 20 * gi, CurrentSizeBytes: &cur}},
	}
	if got := report.Build(in, report.Options{}).Pools[0].UsedBytes; got != 21479030784 {
		t.Errorf("used with currentSize = %d", got)
	}
	in.LVs[0].CurrentSizeBytes = nil
	if got := report.Build(in, report.Options{}).Pools[0].UsedBytes; got != 21474836480 {
		t.Errorf("used without currentSize = %d", got)
	}
}

func TestBreaches(t *testing.T) {
	r := build(load(t, "single-thick"))
	got := report.Breaches(r, 20, 0)
	if len(got) != 1 || got[0].Node != "node-a" || got[0].Message != "node-a/ssd: 20.9% used > 20% (113.5Gi free)" {
		t.Errorf("fail-over 20: %+v", got)
	}
	got = report.Breaches(r, 0, 100*gi)
	if len(got) != 1 || got[0].Message != "node-b/ssd: 60Gi free < 100Gi" {
		t.Errorf("min-free 100Gi: %+v", got)
	}
	thin := build(load(t, "multi-class"))
	for _, b := range report.Breaches(thin, 1, 0) {
		if b.DeviceClass == "nvme-thin" {
			t.Errorf("thin pools are not checked by USE%%: %+v", b)
		}
	}
	want := []string{`--fail-over does not check thin class "nvme-thin" without --metrics: its USE% is virtual`}
	if got := report.FailOverGaps(thin, 1); !reflect.DeepEqual(got, want) {
		t.Errorf("gaps = %q", got)
	}
	for i := range thin.Pools {
		if thin.Pools[i].DeviceClass == "nvme-thin" {
			thin.Pools[i].ThinDataPercent = new(92.5)
		}
	}
	found := false
	for _, b := range report.Breaches(thin, 90, 0) {
		found = found || b.Message == "node-a/nvme-thin: thin pool data 92.5% > 90%"
	}
	if !found || len(report.FailOverGaps(thin, 90)) != 0 {
		t.Errorf("with --metrics --fail-over checks the data use of a thin pool: %+v", report.Breaches(thin, 90, 0))
	}
}

func TestPoolsWithMetrics(t *testing.T) {
	in := load(t, "single-thick")
	vg := int64(510610374656)
	in.Metrics = map[string]map[string]report.ClassMetrics{"node-a": {"ssd": {VGSizeBytes: &vg}}}
	p := build(in).Pools[0]
	want := vg - 10*gi - 121915834368 - 30*gi
	if i64(p.VGSizeBytes) != fmt.Sprint(vg) || i64(p.OtherBytes) != fmt.Sprint(want) {
		t.Errorf("vg = %s other = %s, want other %d", i64(p.VGSizeBytes), i64(p.OtherBytes), want)
	}
	if q := build(in).Pools[1]; q.VGSizeBytes != nil || q.OtherBytes != nil {
		t.Errorf("node-b has no metrics: %+v", q)
	}
}

func TestVolumesWithUsage(t *testing.T) {
	in := load(t, "single-thick")
	used, capacity := 2*gi, 10*gi
	in.Usage = map[string]report.VolumeUsage{"ns-a/data-1": {UsedBytes: &used, CapacityBytes: &capacity}}
	v := build(in).Volumes[0]
	if i64(v.FSUsedBytes) != fmt.Sprint(2*gi) || i64(v.IdleBytes) != fmt.Sprint(8*gi) || i64(v.FSCapacityBytes) != fmt.Sprint(10*gi) {
		t.Errorf("volume = %+v", v)
	}
	if w := build(in).Volumes[1]; w.FSUsedBytes != nil || w.IdleBytes != nil {
		t.Errorf("no usage for data-2: %+v", w)
	}
}

func TestUnknownTypeWarningWhenConfigMapLacksClass(t *testing.T) {
	r := build(load(t, "consistency"))
	want := `type of device class "old" is unknown (not in the lvmd config); USE% assumes thick, pass --thin=old if it is a thin pool`
	found := false
	for _, w := range r.Warnings {
		found = found || w == want
	}
	if !found {
		t.Errorf("warnings = %q", r.Warnings)
	}
}

func TestOtherWithSharedThinVG(t *testing.T) {
	spare := int64(0)
	vg, pool := 100*gi, 30*gi
	in := report.Input{
		Driver: topolvm.Current,
		Nodes:  []topolvm.Node{{Name: "n", Capacity: map[string]int64{"00default": 50 * gi, "t": 50 * gi, "th": 200 * gi}}},
		Classes: map[string]topolvm.DeviceClass{
			"t":  {Name: "t", VolumeGroup: "vg0", Type: topolvm.TypeThick, Default: true, SpareBytes: &spare},
			"th": {Name: "th", VolumeGroup: "vg0", Type: topolvm.TypeThin, OverprovisionRatio: 10},
		},
		LVs:     []topolvm.LogicalVolume{{Name: "a", UID: "a", VolumeID: "a", NodeName: "n", DeviceClass: "t", SizeBytes: 10 * gi}},
		Metrics: map[string]map[string]report.ClassMetrics{"n": {"t": {VGSizeBytes: &vg}, "th": {VGSizeBytes: &vg, ThinPoolSizeBytes: &pool}}},
	}
	p := report.Build(in, report.Options{}).Pools[0]
	if p.DeviceClass != "t" || i64(p.OtherBytes) != fmt.Sprint(10*gi) {
		t.Errorf("OTHER must leave out the thin pool sharing vg0: %+v other=%s", p, i64(p.OtherBytes))
	}
	delete(in.Metrics["n"], "th")
	if p := report.Build(in, report.Options{}).Pools[0]; p.OtherBytes != nil {
		t.Errorf("without the thin pool size OTHER is unknown, got %s", i64(p.OtherBytes))
	}
}

func TestPoolsUsePerNodeClasses(t *testing.T) {
	spareA, spareB := int64(1)<<30, int64(2)<<30
	in := report.Input{
		Driver: topolvm.Current,
		Nodes: []topolvm.Node{
			{Name: "w1", Capacity: map[string]int64{"00default": 10 * gi, "ssd": 10 * gi}},
			{Name: "w2", Capacity: map[string]int64{"00default": 20 * gi, "ssd": 20 * gi, "hdd": 5 * gi}},
		},
		Classes: map[string]topolvm.DeviceClass{"ssd": {Name: "ssd", Type: topolvm.TypeThick, Default: true}, "hdd": {Name: "hdd", Type: topolvm.TypeThick}},
		NodeClasses: map[string]map[string]topolvm.DeviceClass{
			"w1": {"ssd": {Name: "ssd", VolumeGroup: "vg-a", Type: topolvm.TypeThick, Default: true, SpareBytes: &spareA}},
			"w2": {"ssd": {Name: "ssd", VolumeGroup: "vg-b", Type: topolvm.TypeThick, Default: true, SpareBytes: &spareB},
				"hdd": {Name: "hdd", VolumeGroup: "vg-h", Type: topolvm.TypeThick}},
		},
	}
	r := report.Build(in, report.Options{})
	got := map[string]string{}
	for _, p := range r.Pools {
		got[p.Node+"/"+p.DeviceClass] = str(p.VolumeGroup) + " " + i64(p.SpareBytes)
	}
	want := map[string]string{"w1/ssd": "vg-a 1073741824", "w2/ssd": "vg-b 2147483648", "w2/hdd": "vg-h nil"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pools = %v, want %v", got, want)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings = %q", r.Warnings)
	}
}

func TestBuildPoolsCountsOnlyLVsOnDisk(t *testing.T) {
	for _, d := range []topolvm.Driver{topolvm.Current, topolvm.Legacy} {
		gone := time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
		in := report.Input{
			Driver: d,
			Nodes:  []topolvm.Node{{Name: "n", Capacity: map[string]int64{"00default": gi, "ssd": gi}}},
			LVs: []topolvm.LogicalVolume{
				{Name: "a", UID: "a", NodeName: "n", SizeBytes: 2 * gi},
				{Name: "b", UID: "b", NodeName: "n", SizeBytes: gi, DeletionTimestamp: &gone, Finalizers: []string{"e2e.topolvm/hold"}},
				{Name: "c", UID: "c", NodeName: "n", SizeBytes: gi, DeletionTimestamp: &gone, Finalizers: []string{d.LVFinalizer(), "e2e.topolvm/hold"}},
				{Name: "failed", UID: "failed", NodeName: "n", SizeBytes: 20 * gi, Code: 8, Message: "not enough space"},
			},
		}
		for i := range in.LVs {
			if in.LVs[i].Name != "failed" {
				in.LVs[i].VolumeID = in.LVs[i].UID
			}
		}
		p := report.Build(in, report.Options{}).Pools[0]
		if p.UsedBytes != 3*gi || p.Volumes != 2 || i64(p.SizeBytes) != fmt.Sprint(4*gi) {
			t.Errorf("%s: used=%d volumes=%d size=%s, want 3Gi, 2 and 4Gi", d.Name, p.UsedBytes, p.Volumes, i64(p.SizeBytes))
		}
	}
}

func TestOtherWhenLvmdClampsFree(t *testing.T) {
	spare := 10 * gi
	vg, pool, raw := 100*gi, 30*gi, 5*gi
	in := report.Input{
		Driver:  topolvm.Current,
		Nodes:   []topolvm.Node{{Name: "n", Capacity: map[string]int64{"00default": 0, "t": 0, "th": 200 * gi}}},
		Classes: map[string]topolvm.DeviceClass{"t": {Name: "t", VolumeGroup: "vg0", Type: topolvm.TypeThick, Default: true, SpareBytes: &spare}},
		LVs:     []topolvm.LogicalVolume{{Name: "a", UID: "a", VolumeID: "a", NodeName: "n", DeviceClass: "t", SizeBytes: 62 * gi}},
		Metrics: map[string]map[string]report.ClassMetrics{"n": {"t": {VGSizeBytes: &vg}}},
	}
	if p := report.Build(in, report.Options{}).Pools[0]; p.OtherBytes != nil {
		t.Errorf("lvmd reports FREE 0 once the VG has less free than spare: OTHER is unknown, got %s", i64(p.OtherBytes))
	}
	in.Classes["th"] = topolvm.DeviceClass{Name: "th", VolumeGroup: "vg0", Type: topolvm.TypeThin, OverprovisionRatio: 10}
	in.Metrics["n"]["th"] = report.ClassMetrics{VGSizeBytes: &vg, VGAvailableBytes: &raw, ThinPoolSizeBytes: &pool}
	if p := report.Build(in, report.Options{}).Pools[0]; i64(p.OtherBytes) != fmt.Sprint(3*gi) {
		t.Errorf("a thin class in the same VG reports its raw free space, so OTHER is exact: %s", i64(p.OtherBytes))
	}
	delete(in.Classes, "th")
	delete(in.Metrics["n"], "th")
	in.Nodes[0].Capacity = map[string]int64{"00default": 50 * gi, "t": 50 * gi}
	if p := report.Build(in, report.Options{}).Pools[0]; p.OtherBytes != nil {
		t.Errorf("FREE plus USED above the VG is a stale annotation, not negative OTHER: %s", i64(p.OtherBytes))
	}
}

func TestNoDefaultClass(t *testing.T) {
	ssdOnly := map[string]topolvm.DeviceClass{"ssd": {Name: "ssd", VolumeGroup: "vg0", Type: topolvm.TypeThick}}
	for _, c := range []struct {
		name     string
		capacity map[string]int64
		classes  map[string]topolvm.DeviceClass
		want     bool
	}{
		{"config without a default", map[string]int64{"00default": 0, "ssd": 50 * gi}, ssdOnly, false},
		{"unknown config, alias 0 and a free class", map[string]int64{"00default": 0, "ssd": 50 * gi}, nil, false},
		{"unknown config, alias equals the class", map[string]int64{"00default": 50 * gi, "ssd": 50 * gi}, nil, true},
		{"unknown config, full default class", map[string]int64{"00default": 0, "ssd": 0}, nil, true},
		{"unknown config, no alias", map[string]int64{"ssd": 50 * gi}, nil, true},
	} {
		in := report.Input{Driver: topolvm.Current, Nodes: []topolvm.Node{{Name: "n", Capacity: c.capacity}}, Classes: c.classes}
		if got := report.Build(in, report.Options{}).Pools[0].Default; got != c.want {
			t.Errorf("%s: default = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFullDefaultClassWithoutConfig(t *testing.T) {
	in := report.Input{
		Driver: topolvm.Current,
		Nodes:  []topolvm.Node{{Name: "n", Capacity: map[string]int64{"00default": 0, "ssd": 0, "hdd": 50 * gi}}},
		LVs:    []topolvm.LogicalVolume{{Name: "a", UID: "a", VolumeID: "a", NodeName: "n", SizeBytes: 100 * gi}},
	}
	r := report.Build(in, report.Options{})
	if got := classes(r.Pools); !reflect.DeepEqual(got, []string{"n/ssd", "n/hdd"}) || r.Pools[0].UsedBytes != 100*gi || !r.Pools[0].Default {
		t.Errorf("volumes without a class on the node show the full class is the default: %v %+v", got, r.Pools[0])
	}
	if b := report.Breaches(r, 90, 0); len(b) != 1 || b[0].DeviceClass != "ssd" {
		t.Errorf("a full default class must breach --fail-over: %+v", b)
	}
	in.LVs = nil
	r = report.Build(in, report.Options{})
	for _, p := range r.Pools {
		if p.Default {
			t.Errorf("without volumes nothing tells a full default class from no default: %+v", p)
		}
	}
}

func TestFailOverGapForPoolsOfUnknownSize(t *testing.T) {
	r := report.Report{Pools: []report.Pool{{Node: "n", DeviceClass: report.DefaultPlaceholder, Type: topolvm.TypeUnknown, UsedBytes: gi}}}
	want := []string{`--fail-over does not check n/` + report.DefaultPlaceholder + `: its size is unknown`}
	if got := report.FailOverGaps(r, 90); !reflect.DeepEqual(got, want) {
		t.Errorf("gaps = %q", got)
	}
}

func TestPoolMathEdges(t *testing.T) {
	in := report.Input{
		Driver: topolvm.Current,
		Nodes: []topolvm.Node{
			{Name: "a", Capacity: map[string]int64{"00default": 10 * gi, "ssd": 10 * gi, "fast": 5 * gi}},
			{Name: "b", Capacity: map[string]int64{"hdd": 5 * gi, "fast": 5 * gi}},
		},
		NodeClasses: map[string]map[string]topolvm.DeviceClass{
			"a": {"ssd": {Name: "ssd", Type: topolvm.TypeThick, Default: true}, "fast": {Name: "fast", Type: topolvm.TypeThin}},
			"b": {"hdd": {Name: "hdd", Type: topolvm.TypeThick, Default: true}, "fast": {Name: "fast", Type: topolvm.TypeThick}},
		},
		LVs: []topolvm.LogicalVolume{
			{Name: "x", UID: "x", VolumeID: "x", NodeName: "a", DeviceClass: "ssd", SizeBytes: 10 * gi},
			{Name: "y", UID: "y", VolumeID: "y", NodeName: "b", DeviceClass: "ssd", SizeBytes: 2 * gi},
		},
	}
	r := report.Build(in, report.Options{})
	types := map[string]string{}
	for _, p := range r.Pools {
		types[p.Node+"/"+p.DeviceClass] = p.Type
	}
	if types["a/fast"] != topolvm.TypeThin || types["b/fast"] != topolvm.TypeThick {
		t.Errorf("each node's lvmd config decides its type: %v", types)
	}
	byClass := map[string]report.Total{}
	for _, tot := range r.Totals {
		byClass[tot.DeviceClass] = tot
	}
	ssd := byClass["ssd"]
	if ssd.SizeBytes != nil || ssd.FreeBytes != nil || i64(ssd.MaxFreeBytes) != fmt.Sprint(10*gi) || ssd.Default || ssd.UsedBytes != 12*gi {
		t.Errorf("a total over a node without the class's free space is unknown, and default only if every row is: %+v", ssd)
	}
	if fast := byClass["fast"]; fast.Type != "unknown" {
		t.Errorf("a class thin on one node and thick on another has no single type: %+v", fast)
	}

	twins := report.Input{Driver: topolvm.Current, Nodes: []topolvm.Node{{Name: "n", Capacity: map[string]int64{"00default": 5 * gi, "a": 5 * gi, "b": 5 * gi}}}}
	for _, p := range report.Build(twins, report.Options{}).Pools {
		if p.Default {
			t.Errorf("an alias equal to two classes names neither: %+v", p)
		}
	}

	edge := report.Report{Pools: []report.Pool{{Node: "n", DeviceClass: "ssd", Type: topolvm.TypeThick, UsedBytes: 85,
		SizeBytes: new(int64(100)), FreeBytes: new(int64(15))}}}
	if b := report.Breaches(edge, 85, 15); len(b) != 0 {
		t.Errorf("exactly at --fail-over and --min-free is not a breach: %+v", b)
	}
	if b := report.Breaches(edge, 84.9, 16); len(b) != 2 {
		t.Errorf("just past both thresholds: %+v", b)
	}
}
