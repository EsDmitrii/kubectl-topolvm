package output

import (
	"fmt"
	"io"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// WritePools prints one row per node and device class, then a * row per class spread over more
// than one node.
func WritePools(w io.Writer, r report.Report, o TableOptions) error {
	t := newTable()
	if !o.NoHeaders {
		h := []string{"NODE", "DEVICE-CLASS", "TYPE", "SIZE", "USED", "FREE", "USE%", "LVS"}
		if o.Wide {
			h = append(h, "VG", "SPARE")
		}
		if o.Metrics {
			h = append(h, "VG-SIZE", "OTHER", "DATA%", "META%")
		}
		t.row(h...)
	}
	for i := range r.Pools {
		p := &r.Pools[i]
		use, color := usePercent(p.Type, p.UsedBytes, p.SizeBytes, o)
		cells := []string{
			p.Node, classLabel(p.DeviceClass, p.Default), typeLabel(p.Type),
			o.sizeOr(p.SizeBytes, unknown), o.size(p.UsedBytes), o.sizeOr(p.FreeBytes, unknown), use, fmt.Sprint(p.Volumes),
		}
		if o.Wide {
			vg := unknown
			if p.VolumeGroup != nil {
				vg = *p.VolumeGroup
			}
			spare := o.sizeOr(p.SpareBytes, unknown)
			if p.Type == topolvm.TypeThin {
				spare = "-"
			}
			cells = append(cells, vg, spare)
		}
		if o.Metrics {
			// "-" where a column does not apply to the pool type, <unknown> where it does.
			other, data, meta := o.sizeOr(p.OtherBytes, unknown), "-", "-"
			if p.Type == topolvm.TypeThin {
				other, data, meta = "-", percentOr(p.ThinDataPercent), percentOr(p.ThinMetadataPercent)
			}
			cells = append(cells, o.sizeOr(p.VGSizeBytes, unknown), other, data, meta)
		}
		t.row(cells...)
		t.paint(6, color)
	}
	if !o.NoHeaders {
		for i := range r.Totals {
			tot := &r.Totals[i]
			use, color := usePercent(tot.Type, tot.UsedBytes, tot.SizeBytes, o)
			cells := []string{
				"*", classLabel(tot.DeviceClass, tot.Default), typeLabel(tot.Type),
				o.sizeOr(tot.SizeBytes, unknown), o.size(tot.UsedBytes), o.sizeOr(tot.FreeBytes, unknown), use, fmt.Sprint(tot.Volumes),
			}
			if o.Wide {
				cells = append(cells, "-", "-")
			}
			if o.Metrics {
				cells = append(cells, "-", "-", "-", "-")
			}
			t.row(cells...)
			t.paint(6, color)
		}
	}
	return t.render(w)
}

func percentOr(p *float64) string {
	if p == nil {
		return unknown
	}
	return fmt.Sprintf("%.1f%%", *p)
}

// WriteGroups prints volumes aggregated by one key.
func WriteGroups(w io.Writer, groups []report.Group, key string, o TableOptions) error {
	t := newTable()
	if !o.NoHeaders {
		h := []string{upper(key), "VOLUMES", "SIZE"}
		if o.Usage {
			h = append(h, "FS-USED", "IDLE")
		}
		t.row(h...)
	}
	for _, g := range groups {
		cells := []string{g.Key, fmt.Sprint(g.Volumes), o.size(g.SizeBytes)}
		if o.Usage {
			cells = append(cells, o.sizeOr(g.UsedBytes, unknown), o.sizeOr(g.IdleBytes, unknown))
		}
		t.row(cells...)
	}
	return t.render(w)
}

func upper(s string) string {
	out := []rune(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 'a' + 'A'
		}
	}
	return string(out)
}

type branches struct{ mid, last, pipe, space string }

func treeGlyphs(ascii bool) branches {
	if ascii {
		return branches{"|- ", `\- `, "|  ", "   "}
	}
	return branches{"├─ ", "└─ ", "│  ", "   "}
}

func pick(isLast bool, last, mid string) string {
	if isLast {
		return last
	}
	return mid
}

// TreePod is a pod that mounts a claim in the tree.
type TreePod struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}

// WriteTree prints node → device class → volume → pod. podsByClaim maps namespace/claim to the pods
// that mount it; nil leaves the pod level out.
func WriteTree(w io.Writer, r report.Report, podsByClaim map[string][]TreePod, o TableOptions) error {
	g := treeGlyphs(o.ASCII)
	t := newTable()
	if !o.NoHeaders {
		t.row("NAME", "SIZE", "USED", "FREE", "USE%", "STATUS")
	}
	var nodes []string
	poolsByNode := map[string][]*report.Pool{}
	for i := range r.Pools {
		p := &r.Pools[i]
		if _, ok := poolsByNode[p.Node]; !ok {
			nodes = append(nodes, p.Node)
		}
		poolsByNode[p.Node] = append(poolsByNode[p.Node], p)
	}
	for _, node := range nodes {
		t.row(node)
		pools := poolsByNode[node]
		for pi, p := range pools {
			lastPool := pi == len(pools)-1
			use, color := usePercent(p.Type, p.UsedBytes, p.SizeBytes, o)
			t.row(pick(lastPool, g.last, g.mid)+classLabel(p.DeviceClass, p.Default),
				o.sizeOr(p.SizeBytes, unknown), o.size(p.UsedBytes), o.sizeOr(p.FreeBytes, unknown), use)
			t.paint(4, color)
			indent := pick(lastPool, g.space, g.pipe)
			var vols []*report.Volume
			for vi := range r.Volumes {
				v := &r.Volumes[vi]
				if v.Node == node && v.DeviceClass == p.DeviceClass {
					vols = append(vols, v)
				}
			}
			for vi, v := range vols {
				lastVol := vi == len(vols)-1
				t.row(indent+pick(lastVol, g.last, g.mid)+volumeLabel(v), o.size(v.SizeBytes), "", "", "", v.Status)
				t.paint(5, statusColor(v.Status, o))
				if v.Namespace == nil || v.PVC == nil {
					continue
				}
				pods := podsByClaim[*v.Namespace+"/"+*v.PVC]
				for k, pod := range pods {
					t.row(indent + pick(lastVol, g.space, g.pipe) + pick(k == len(pods)-1, g.last, g.mid) + "pod/" + pod.Name + " " + pod.Phase)
				}
			}
		}
	}
	return t.render(w)
}

func volumeLabel(v *report.Volume) string {
	switch {
	case v.Namespace != nil && v.PVC != nil:
		return *v.Namespace + "/" + *v.PVC
	case v.Kind == report.KindSnapshot:
		return "<snapshot> " + v.LV
	case v.Status == "Orphan":
		return "<orphan> " + v.LV
	default:
		return v.LV
	}
}
