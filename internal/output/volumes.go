package output

import (
	"fmt"
	"io"
	"time"

	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

func statusColor(status string, o TableOptions) string {
	if !o.Color {
		return ""
	}
	switch status {
	case "Orphan", "Error", "Failed":
		return red
	case "Resizing", "Deleting", "Pending", "Released":
		return yellow
	default:
		return ""
	}
}

// WriteVolumes prints one row per LogicalVolume.
func WriteVolumes(w io.Writer, r report.Report, o TableOptions) error {
	t := newTable()
	if !o.NoHeaders {
		h := []string{"NODE", "DEVICE-CLASS", "SIZE", "NAMESPACE", "PVC", "STORAGECLASS", "STATUS", "AGE"}
		if o.Usage {
			h = append(h, "FS-USED", "FS-USE%", "IDLE")
		}
		if o.Wide {
			h = append(h, "KIND", "CURRENT", "RECLAIM", "LV", "VOLUME-ID")
		}
		t.row(h...)
	}
	for i := range r.Volumes {
		v := &r.Volumes[i]
		cells := []string{
			v.Node, v.DeviceClass, o.size(v.SizeBytes), orNone(v.Namespace), orNone(v.PVC),
			orNone(v.StorageClass), v.Status, age(r.GeneratedAt, v.CreatedAt),
		}
		if o.Usage {
			cells = append(cells, o.sizeOr(v.FSUsedBytes, unknown), fsPercent(v), o.sizeOr(v.IdleBytes, unknown))
		}
		if o.Wide {
			volID := v.VolumeID
			if volID == "" {
				volID = none
			}
			cells = append(cells, v.Kind, o.sizeOr(v.CurrentSizeBytes, none), orNone(v.ReclaimPolicy), v.LV, volID)
		}
		t.row(cells...)
		t.paint(6, statusColor(v.Status, o))
	}
	return t.render(w)
}

func fsPercent(v *report.Volume) string {
	if v.FSUsedBytes == nil || v.FSCapacityBytes == nil || *v.FSCapacityBytes == 0 {
		return unknown
	}
	return fmt.Sprintf("%.0f%%", float64(*v.FSUsedBytes)/float64(*v.FSCapacityBytes)*100)
}

// WriteFindings prints one row per finding.
func WriteFindings(w io.Writer, r report.Report, o TableOptions) error {
	t := newTable()
	if !o.NoHeaders {
		t.row("CHECK", "OBJECT", "NODE", "SIZE", "AGE", "DETAIL")
	}
	for _, f := range r.Findings {
		ageCell := none
		if f.AgeSeconds != nil {
			ageCell = duration.HumanDuration(time.Duration(*f.AgeSeconds) * time.Second)
		}
		node := f.Node
		if node == "" {
			node = none
		}
		t.row(f.Check, f.Object, node, o.sizeOr(f.SizeBytes, none), ageCell, f.Detail)
	}
	return t.render(w)
}

// WritePending prints one row per unbound TopoLVM claim.
func WritePending(w io.Writer, rows []report.PendingPVC, now time.Time, o TableOptions) error {
	t := newTable()
	if !o.NoHeaders {
		h := []string{"NAMESPACE", "PVC", "SIZE", "STORAGECLASS", "DEVICE-CLASS", "NODE", "AGE", "REASON", "DETAIL"}
		if o.Wide {
			h = append(h, "LAST-EVENT")
		}
		t.row(h...)
	}
	for i := range rows {
		r := &rows[i]
		node := r.SelectedNode
		if node == "" {
			node = none
		}
		cells := []string{r.Namespace, r.PVC, o.size(r.RequestBytes), r.StorageClass, r.DeviceClass, node,
			age(now, r.Created), r.Reason, r.Detail}
		if o.Wide {
			cells = append(cells, orDash(r.LastEvent))
		}
		t.row(cells...)
	}
	return t.render(w)
}
