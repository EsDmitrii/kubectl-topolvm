package report

import (
	"fmt"
	"sort"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// defaultClass tells which device class is the default on a node: the one the lvmd config marks
// default, else the named class whose free bytes equal the 00default alias, else, without an lvmd
// config, the only class unless the alias contradicts it. lvmd fills the alias only from a class
// marked default, so a config without one means no default. An alias of 0 matches a full default
// class or none at all; only volumes without a class on the node (emptyLVs) tell them apart.
func defaultClass(n topolvm.Node, classes map[string]topolvm.DeviceClass, emptyLVs func() bool) (string, bool) {
	var marked []string
	for name, dc := range classes {
		if dc.Default {
			marked = append(marked, name)
		}
	}
	if len(marked) == 1 {
		return marked[0], true
	}
	named := n.NamedClasses()
	if alias, ok := n.Capacity[topolvm.DefaultAlias]; ok && (alias > 0 || (len(classes) == 0 && emptyLVs != nil)) {
		var match []string
		for _, c := range named {
			if n.Capacity[c] == alias {
				match = append(match, c)
			}
		}
		if len(match) == 1 && (alias > 0 || emptyLVs()) {
			return match[0], true
		}
	}
	if len(named) == 1 && len(classes) == 0 {
		if alias, ok := n.Capacity[topolvm.DefaultAlias]; !ok || alias == n.Capacity[named[0]] {
			return named[0], true
		}
	}
	return "", false
}

// resolveClass maps an LV's deviceClass to the class it belongs to on its node.
func resolveClass(lv *topolvm.LogicalVolume, def string, defOK bool) string {
	switch {
	case lv.DeviceClass != "":
		return lv.DeviceClass
	case defOK:
		return def
	default:
		return DefaultPlaceholder
	}
}

// classTypes decides thick or thin per node and class: --thin wins, then the node's lvmd config,
// then any LV cut from another one (only thin pools have snapshots and clones).
func classTypes(in *Input) func(node, class string) string {
	thin := set(in.Thin)
	withSource := map[string]bool{}
	for i := range in.LVs {
		if in.LVs[i].Source != "" && in.LVs[i].DeviceClass != "" {
			withSource[in.LVs[i].DeviceClass] = true
		}
	}
	defaults := map[string]bool{}
	for _, n := range in.Nodes {
		if d, ok := in.defaultOf(n); ok {
			defaults[d] = true
		}
	}
	for i := range in.LVs {
		if in.LVs[i].Source != "" && in.LVs[i].DeviceClass == "" {
			for d := range defaults {
				withSource[d] = true
			}
		}
	}
	return func(node, class string) string {
		switch {
		case class == "" || class == DefaultPlaceholder:
			return topolvm.TypeUnknown
		case thin[class]:
			return topolvm.TypeThin
		}
		if dc, ok := in.classesOf(node)[class]; ok {
			return dc.Type
		}
		if withSource[class] {
			return topolvm.TypeThin
		}
		return topolvm.TypeUnknown
	}
}

func buildPools(in Input, o Options) (pools []Pool, warnings []string) {
	nodeFilter, classFilter := set(o.Nodes), set(o.DeviceClasses)
	typeOf := classTypes(&in)

	nodes := append([]topolvm.Node(nil), in.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	pools = []Pool{}
	var nodeWarnings []string
	seenClasses := map[string]string{}
	lvsByNode := map[string][]int{}
	for i := range in.LVs {
		lvsByNode[in.LVs[i].NodeName] = append(lvsByNode[in.LVs[i].NodeName], i)
	}
	for _, n := range nodes {
		if !allowed(nodeFilter, n.Name) {
			continue
		}
		nodeClasses := in.classesOf(n.Name)
		def, defOK := in.defaultOf(n)
		used := map[string]int64{}
		count := map[string]int{}
		emptyClass := false
		for _, i := range lvsByNode[n.Name] {
			lv := &in.LVs[i]
			if !lv.OnDisk(in.Driver.LVFinalizer()) {
				continue
			}
			if lv.DeviceClass == "" {
				emptyClass = true
			}
			c := resolveClass(lv, def, defOK)
			used[c] += lv.UsedBytes()
			count[c]++
		}
		names := n.NamedClasses()
		for c := range used {
			if _, ok := n.Capacity[c]; !ok {
				names = append(names, c)
			}
		}
		sort.Slice(names, func(i, j int) bool {
			if (names[i] == def && defOK) != (names[j] == def && defOK) {
				return names[i] == def && defOK
			}
			return names[i] < names[j]
		})

		if emptyClass && !defOK {
			nodeWarnings = append(nodeWarnings, fmt.Sprintf(
				"node %s: cannot tell the default device class; volumes without deviceClass are shown as %s", n.Name, DefaultPlaceholder))
		}
		for _, bad := range n.BadAnnotations {
			nodeWarnings = append(nodeWarnings, fmt.Sprintf("node %s: ignoring unparsable annotation %s", n.Name, bad))
		}
		if len(n.Capacity) == 0 {
			nodeWarnings = append(nodeWarnings, fmt.Sprintf(
				"node %s has no %s* annotations: is topolvm-node running there?", n.Name, in.Driver.CapacityPrefix()))
		}
		if len(names) == 0 {
			if classFilter == nil {
				pools = append(pools, Pool{Node: n.Name, Type: topolvm.TypeUnknown})
			}
			continue
		}

		for _, c := range names {
			if !allowed(classFilter, c) {
				continue
			}
			p := Pool{
				Node:        n.Name,
				DeviceClass: c,
				Default:     defOK && c == def,
				Type:        typeOf(n.Name, c),
				UsedBytes:   used[c],
				Volumes:     count[c],
			}
			if dc, ok := nodeClasses[c]; ok {
				if dc.VolumeGroup != "" {
					p.VolumeGroup = new(dc.VolumeGroup)
				}
				p.SpareBytes = dc.SpareBytes
			}
			if free, ok := n.Capacity[c]; ok && c != DefaultPlaceholder {
				p.FreeBytes = new(free)
				p.SizeBytes = new(free + p.UsedBytes)
				if *p.SizeBytes > 0 && p.Type != topolvm.TypeThin {
					p.UsedPercent = new(percent(p.UsedBytes, *p.SizeBytes))
				}
			}
			applyMetrics(&p, in.Metrics[n.Name], nodeClasses)
			pools = append(pools, p)
			seenClasses[c] = p.Type
		}
	}

	classNames := make([]string, 0, len(seenClasses))
	for c := range seenClasses {
		classNames = append(classNames, c)
	}
	sort.Strings(classNames)
	warnings = nodeWarnings
	if !o.SizeNotes {
		return pools, warnings
	}
	for _, c := range classNames {
		switch seenClasses[c] {
		case topolvm.TypeUnknown:
			if c != DefaultPlaceholder {
				why := "no readable lvmd config"
				if in.Classes != nil || in.NodeClasses != nil {
					why = "not in the lvmd config"
				}
				warnings = append(warnings, fmt.Sprintf(
					"type of device class %q is unknown (%s); USE%% assumes thick, pass --thin=%s if it is a thin pool", c, why, c))
			}
		case topolvm.TypeThin:
			warnings = append(warnings, fmt.Sprintf(
				"device class %q is thin: SIZE, USED and FREE are virtual (overprovisioned) sizes", c))
		}
	}
	return pools, warnings
}

func buildTotals(pools []Pool) []Total {
	byClass := map[string][]Pool{}
	for _, p := range pools {
		if p.DeviceClass != "" {
			byClass[p.DeviceClass] = append(byClass[p.DeviceClass], p)
		}
	}
	totals := []Total{}
	for class, rows := range byClass {
		if len(rows) < 2 {
			continue
		}
		t := Total{DeviceClass: class, Default: true, Type: rows[0].Type, Nodes: len(rows)}
		var size, free int64
		known := true
		for _, p := range rows {
			t.Default = t.Default && p.Default
			if p.Type != t.Type {
				t.Type = "unknown"
			}
			t.UsedBytes += p.UsedBytes
			t.Volumes += p.Volumes
			if p.FreeBytes == nil {
				known = false
				continue
			}
			size += *p.SizeBytes
			free += *p.FreeBytes
			if t.MaxFreeBytes == nil || *p.FreeBytes > *t.MaxFreeBytes {
				t.MaxFreeBytes = new(*p.FreeBytes)
				t.MaxFreeNode = p.Node
			}
		}
		if known {
			t.SizeBytes, t.FreeBytes = new(size), new(free)
			if size > 0 && t.Type != "thin" {
				t.UsedPercent = new(percent(t.UsedBytes, size))
			}
		}
		totals = append(totals, t)
	}
	sort.Slice(totals, func(i, j int) bool {
		if totals[i].Default != totals[j].Default {
			return totals[i].Default
		}
		return totals[i].DeviceClass < totals[j].DeviceClass
	})
	return totals
}

// applyMetrics adds what only topolvm-node metrics know: the size of the volume group, the space in
// it TopoLVM does not manage (OTHER, thick only) and thin pool fill. OTHER is VG - spare - FREE - USED,
// less any thin pool sharing the volume group; unknown without that pool's size, or when lvmd
// reports FREE 0 and no thin class in the VG gives its raw free space.
func applyMetrics(p *Pool, nodeMetrics map[string]ClassMetrics, classes map[string]topolvm.DeviceClass) {
	m := nodeMetrics[p.DeviceClass]
	p.VGSizeBytes = m.VGSizeBytes
	if p.Type == topolvm.TypeThin {
		p.ThinDataPercent, p.ThinMetadataPercent = m.ThinDataPercent, m.ThinMetadataPercent
		return
	}
	if m.VGSizeBytes == nil || p.SpareBytes == nil || p.FreeBytes == nil {
		return
	}
	// A thin pool in the same VG is TopoLVM's too. lvmd reports the raw VG free space for a thin
	// class but VG free minus spare, floored at 0, for a thick one.
	vg := classes[p.DeviceClass].VolumeGroup
	var pools int64
	var rawFree *int64
	for name, dc := range classes {
		if name == p.DeviceClass || dc.Type != topolvm.TypeThin {
			continue
		}
		if vg == "" {
			return
		}
		if dc.VolumeGroup != vg {
			continue
		}
		tm := nodeMetrics[name]
		if tm.ThinPoolSizeBytes == nil {
			return
		}
		pools += *tm.ThinPoolSizeBytes
		if tm.VGAvailableBytes != nil {
			rawFree = tm.VGAvailableBytes
		}
	}
	var other int64
	switch {
	case rawFree != nil:
		other = *m.VGSizeBytes - *rawFree - p.UsedBytes - pools
	case *p.FreeBytes == 0:
		return
	default:
		other = *m.VGSizeBytes - *p.SpareBytes - *p.FreeBytes - p.UsedBytes - pools
	}
	if other >= 0 {
		p.OtherBytes = new(other)
	}
}
