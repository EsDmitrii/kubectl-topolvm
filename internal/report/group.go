package report

import (
	"fmt"
	"sort"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
	"github.com/EsDmitrii/kubectl-topolvm/internal/units"
)

// GroupKeys are the keys volumes can be grouped by.
var GroupKeys = []string{"namespace", "storageclass", "node", "device-class"}

// Group sums volumes sharing one key value.
type Group struct {
	Key       string `json:"key"`
	Volumes   int    `json:"volumes"`
	SizeBytes int64  `json:"sizeBytes"`
	UsedBytes *int64 `json:"fsUsedBytes"`
	IdleBytes *int64 `json:"idleBytes"`
}

func groupKey(v *Volume, key string) string {
	var p *string
	switch key {
	case "namespace":
		p = v.Namespace
	case "storageclass":
		p = v.StorageClass
	case "node":
		return v.Node
	case "device-class":
		return v.DeviceClass
	}
	if p == nil {
		return "<none>"
	}
	return *p
}

// GroupVolumes aggregates volumes by key, largest group first.
func GroupVolumes(vols []Volume, key string) []Group {
	byKey := map[string]*Group{}
	var order []string
	for i := range vols {
		v := &vols[i]
		k := groupKey(v, key)
		g, ok := byKey[k]
		if !ok {
			g = &Group{Key: k}
			byKey[k] = g
			order = append(order, k)
		}
		g.Volumes++
		g.SizeBytes += v.SizeBytes
		if v.FSUsedBytes != nil {
			g.UsedBytes = new(orZero(g.UsedBytes) + *v.FSUsedBytes)
		}
		if v.IdleBytes != nil {
			g.IdleBytes = new(orZero(g.IdleBytes) + *v.IdleBytes)
		}
	}
	out := make([]Group, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SizeBytes != out[j].SizeBytes {
			return out[i].SizeBytes > out[j].SizeBytes
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func orZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Breach is a pool over a threshold given on the command line.
type Breach struct {
	Node        string `json:"node"`
	DeviceClass string `json:"deviceClass"`
	Message     string `json:"message"`
}

// Breaches checks every node row against --fail-over (percent, 0 is off) and --min-free (bytes,
// 0 is off). USE% of a thin pool is virtual: --fail-over checks its data use from --metrics.
func Breaches(r Report, failOver float64, minFree int64) []Breach {
	out := []Breach{}
	for i := range r.Pools {
		p := &r.Pools[i]
		where := p.Node + "/" + p.DeviceClass
		switch {
		case failOver <= 0:
		case p.Type == topolvm.TypeThin:
			if p.ThinDataPercent != nil && *p.ThinDataPercent > failOver {
				out = append(out, Breach{p.Node, p.DeviceClass, fmt.Sprintf("%s: thin pool data %.1f%% > %g%%",
					where, *p.ThinDataPercent, failOver)})
			}
		case p.SizeBytes != nil && *p.SizeBytes > 0 && p.FreeBytes != nil:
			pct := float64(p.UsedBytes) / float64(*p.SizeBytes) * 100
			if pct > failOver {
				out = append(out, Breach{p.Node, p.DeviceClass, fmt.Sprintf("%s: %.1f%% used > %g%% (%s free)",
					where, pct, failOver, units.FormatIEC(*p.FreeBytes))})
			}
		}
		if minFree > 0 && p.FreeBytes != nil && *p.FreeBytes < minFree {
			out = append(out, Breach{p.Node, p.DeviceClass, fmt.Sprintf("%s: %s free < %s",
				where, units.FormatIEC(*p.FreeBytes), units.FormatIEC(minFree))})
		}
	}
	return out
}

// FailOverGaps names the thin classes --fail-over could not check, one line per class.
func FailOverGaps(r Report, failOver float64) []string {
	out := []string{}
	if failOver <= 0 {
		return out
	}
	seen := map[string]bool{}
	for i := range r.Pools {
		p := &r.Pools[i]
		switch {
		case p.Type == topolvm.TypeThin:
			if p.ThinDataPercent == nil && !seen[p.DeviceClass] {
				seen[p.DeviceClass] = true
				out = append(out, fmt.Sprintf("--fail-over does not check thin class %q without --metrics: its USE%% is virtual", p.DeviceClass))
			}
		case p.SizeBytes == nil && p.UsedBytes > 0:
			out = append(out, fmt.Sprintf("--fail-over does not check %s/%s: its size is unknown", p.Node, p.DeviceClass))
		}
	}
	return out
}
