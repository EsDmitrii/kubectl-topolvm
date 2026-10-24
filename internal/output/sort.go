package output

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

// Sort keys. Numeric keys sort largest first, text keys alphabetically.
var (
	PoolSortKeys   = []string{"node", "device-class", "size", "used", "free", "use%", "lvs"}
	VolumeSortKeys = []string{"node", "namespace", "pvc", "size", "age", "status", "fs-used", "idle"}
)

func unknownKey(key string, keys []string) error {
	return fmt.Errorf("unknown sort key %q; want one of %s", key, strings.Join(keys, ", "))
}

func orZero(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func usage(p *report.Pool) float64 {
	if p.SizeBytes == nil || *p.SizeBytes == 0 || p.Type == "thin" {
		return -1
	}
	return float64(p.UsedBytes) / float64(*p.SizeBytes)
}

// SortPools orders the node rows; total rows stay at the end in their own order.
func SortPools(r *report.Report, key string) error {
	var less func(a, b *report.Pool) bool
	switch key {
	case "", "node":
		less = func(a, b *report.Pool) bool { return a.Node < b.Node }
	case "device-class":
		less = func(a, b *report.Pool) bool { return a.DeviceClass < b.DeviceClass }
	case "size":
		less = func(a, b *report.Pool) bool { return orZero(a.SizeBytes) > orZero(b.SizeBytes) }
	case "used":
		less = func(a, b *report.Pool) bool { return a.UsedBytes > b.UsedBytes }
	case "free":
		less = func(a, b *report.Pool) bool { return orZero(a.FreeBytes) > orZero(b.FreeBytes) }
	case "use%":
		less = func(a, b *report.Pool) bool { return usage(a) > usage(b) }
	case "lvs":
		less = func(a, b *report.Pool) bool { return a.Volumes > b.Volumes }
	default:
		return unknownKey(key, PoolSortKeys)
	}
	if key != "" {
		sort.SliceStable(r.Pools, func(i, j int) bool { return less(&r.Pools[i], &r.Pools[j]) })
	}
	return nil
}

// CheckVolumeSortKey rejects an unknown key, and the usage keys without --usage, before the cluster
// is read.
func CheckVolumeSortKey(key string, usage bool) error {
	switch {
	case key == "":
		return nil
	case !slices.Contains(VolumeSortKeys, key):
		return unknownKey(key, VolumeSortKeys)
	case (key == "fs-used" || key == "idle") && !usage:
		return fmt.Errorf("--sort-by %s needs --usage", key)
	}
	return nil
}

// SortVolumes orders the volume rows.
func SortVolumes(r *report.Report, key string) error {
	var less func(a, b *report.Volume) bool
	switch key {
	case "", "node":
		less = func(a, b *report.Volume) bool { return a.Node < b.Node }
	case "namespace":
		less = func(a, b *report.Volume) bool { return orNone(a.Namespace) < orNone(b.Namespace) }
	case "pvc":
		less = func(a, b *report.Volume) bool { return orNone(a.PVC) < orNone(b.PVC) }
	case "size":
		less = func(a, b *report.Volume) bool { return a.SizeBytes > b.SizeBytes }
	case "age":
		less = func(a, b *report.Volume) bool { return a.CreatedAt.Before(b.CreatedAt) }
	case "status":
		less = func(a, b *report.Volume) bool { return a.Status < b.Status }
	case "fs-used":
		less = func(a, b *report.Volume) bool { return orZero(a.FSUsedBytes) > orZero(b.FSUsedBytes) }
	case "idle":
		less = func(a, b *report.Volume) bool { return orZero(a.IdleBytes) > orZero(b.IdleBytes) }
	default:
		return unknownKey(key, VolumeSortKeys)
	}
	if key != "" {
		sort.SliceStable(r.Volumes, func(i, j int) bool { return less(&r.Volumes[i], &r.Volumes[j]) })
	}
	return nil
}
