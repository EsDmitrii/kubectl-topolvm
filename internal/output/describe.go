package output

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/EsDmitrii/kubectl-topolvm/internal/lvsdiff"
	"github.com/EsDmitrii/kubectl-topolvm/internal/rbac"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

// WriteFit prints the candidate nodes for a new volume.
func WriteFit(w io.Writer, res report.FitResult, o TableOptions) error {
	t := newTable()
	if !o.NoHeaders {
		t.row("NODE", "DEVICE-CLASS", "FREE", "FREE-AFTER", "FITS", "COUNT", "NOTES")
	}
	for _, r := range res.Rows {
		t.row(r.Node, r.DeviceClass, o.size(r.FreeBytes), o.sizeOr(r.FreeAfterBytes, "-"), yesNo(r.Fits), fmt.Sprint(r.Count), joinOr(r.Notes))
	}
	return t.render(w)
}

// WriteHeadroom prints how far one claim can grow.
func WriteHeadroom(w io.Writer, h report.Headroom, o TableOptions) error {
	pairs := [][2]string{
		{"PVC", h.Namespace + "/" + h.PVC},
		{"PV", h.PV},
		{"LV", h.LV},
		{"NODE", h.Node},
		{"DEVICE-CLASS", h.DeviceClass + " (" + typeLabel(h.Type) + ")"},
		{"STORAGECLASS", orDash(h.StorageClass)},
		{"SIZE", o.size(h.CurrentBytes)},
		{"FREE ON NODE", o.sizeOr(h.FreeBytes, unknown)},
		{"CAN GROW TO", o.sizeOr(h.MaxBytes, unknown)},
		{"ALLOW EXPANSION", yesNo(h.AllowExpansion)},
	}
	if h.RequestedBytes != nil {
		fits := unknown
		if h.Fits != nil {
			fits = yesNo(*h.Fits)
		}
		pairs = append(pairs, [2]string{"REQUESTED", o.size(*h.RequestedBytes)}, [2]string{"FITS", fits})
	}
	keys := make([]string, 0, len(h.Autoresizer))
	for k := range h.Autoresizer {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pairs = append(pairs, [2]string{"AUTORESIZER", k + "=" + h.Autoresizer[k]})
	}
	pairs = append(pairs, [2]string{"LAST RESIZE FAILURE", orDash(h.LastResizeFailure)})
	return keyValues(w, pairs)
}

// WriteNode prints what a node holds, what deleting it removes and where its volumes could go.
func WriteNode(w io.Writer, nr report.NodeReport, driver string, o TableOptions) error {
	pairs := [][2]string{{"NODE", nr.Node}}
	if s := nr.State; s != nil {
		taints := make([]string, 0, len(s.Taints))
		for _, t := range s.Taints {
			kv := t.Key
			if t.Value != "" {
				kv += "=" + t.Value
			}
			taints = append(taints, kv+":"+string(t.Effect))
		}
		finalizer := driver + "/node"
		if !s.HasFinalizer {
			finalizer = "missing: deleting the Node leaves its LogicalVolumes behind"
		}
		pairs = append(pairs, [2]string{"READY", yesNo(s.Ready)}, [2]string{"UNSCHEDULABLE", yesNo(s.Unschedulable)},
			[2]string{"TAINTS", joinOr(taints)}, [2]string{"FINALIZER", finalizer})
	}
	if err := keyValues(w, pairs); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	if err := WritePools(w, report.Report{Pools: nr.Pools}, TableOptions{Bytes: o.Bytes, Color: o.Color}); err != nil {
		return err
	}
	t := newTable()
	t.row("NAMESPACE", "PVC", "SIZE", "DEVICE-CLASS", "STATUS", "RECLAIM", "OWNER", "PODS", "SINGLE-COPY")
	for i := range nr.Volumes {
		v := &nr.Volumes[i]
		owner := v.Owner
		if owner == "" {
			owner = none
		}
		t.row(orNone(v.Namespace), orNone(v.PVC), o.size(v.SizeBytes), v.DeviceClass, v.Status,
			orNone(v.ReclaimPolicy), owner, joinOr(v.Pods), yesNoUnknown(v.SingleCopy))
		t.paint(4, statusColor(v.Status, o))
		if v.SingleCopy != nil && *v.SingleCopy && o.Color {
			t.paint(8, yellow)
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	if err := t.render(w); err != nil {
		return err
	}
	var b strings.Builder
	writeNodeDeletion(&b, nr, driver)
	b.WriteString("\nEvacuation, first fit by size on Ready schedulable nodes; affinity is not considered:\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	e := newTable()
	e.row("PVC", "SIZE", "DEVICE-CLASS", "TARGET")
	for _, p := range nr.Evacuation {
		target := p.Target
		if target == "" {
			target = "no room"
		}
		e.row(p.Volume, o.size(p.SizeBytes), p.DeviceClass, target)
		if p.Target == "" && o.Color {
			e.paint(3, red)
		}
	}
	return e.render(w)
}

// WritePermissions prints the result of rbac --check.
func WritePermissions(w io.Writer, results []rbac.Result, o TableOptions) error {
	t := newTable()
	if !o.NoHeaders {
		t.row("VERB", "RESOURCE", "NAMESPACE", "ALLOWED")
	}
	for _, r := range results {
		res := r.Resource
		if r.Group != "" {
			res += "." + r.Group
		}
		ns := r.Namespace
		if ns == "" {
			ns = "*"
		}
		t.row(r.Verb, res, ns, yesNo(r.Allowed))
		if !r.Allowed && o.Color {
			t.paint(3, red)
		}
	}
	return t.render(w)
}

// WriteLVSDiff prints the comparison of LogicalVolumes with lvs.
func WriteLVSDiff(w io.Writer, rows []lvsdiff.Row, o TableOptions) error {
	t := newTable()
	if !o.NoHeaders {
		t.row("STATE", "LV", "VG", "DISK-SIZE", "CR-SIZE", "PVC")
	}
	for _, r := range rows {
		t.row(r.State, r.LV, r.VG, o.sizeOr(r.DiskBytes, "-"), o.sizeOr(r.CRBytes, "-"), r.PVC)
		if o.Color && r.State != lvsdiff.OK && r.State != lvsdiff.Foreign {
			t.paint(0, red)
		}
	}
	return t.render(w)
}

// writeNodeDeletion says what the TopoLVM controller does when the Node object is deleted.
func writeNodeDeletion(b *strings.Builder, nr report.NodeReport, driver string) {
	switch n := len(nr.DeletedOnNodeDelete); {
	case nr.State != nil && !nr.State.HasFinalizer:
		fmt.Fprintf(b, "\nkubectl delete node %s would make TopoLVM delete nothing: the Node has no %s/node finalizer\n", nr.Node, driver)
		return
	case nr.DeletedOnNodeDelete == nil:
		fmt.Fprintf(b, "\nkubectl delete node %s would make TopoLVM delete its %d LogicalVolumes and claims that are unknown: "+
			"persistentvolumeclaims or storageclasses were not collected\n", nr.Node, nr.LogicalVolumesDeleted)
		return
	case n == 0:
		fmt.Fprintf(b, "\nkubectl delete node %s would not make TopoLVM delete any PVC\n", nr.Node)
	case n == 1:
		fmt.Fprintf(b, "\nkubectl delete node %s would make TopoLVM delete 1 PVC:\n", nr.Node)
	default:
		fmt.Fprintf(b, "\nkubectl delete node %s would make TopoLVM delete %d PVCs:\n", nr.Node, n)
	}
	for _, c := range nr.DeletedOnNodeDelete {
		fmt.Fprintf(b, "  %s\n", c)
	}
	if nr.LogicalVolumesDeleted == 0 {
		return
	}
	noun := "LogicalVolumes"
	if nr.LogicalVolumesDeleted == 1 {
		noun = "LogicalVolume"
	}
	fmt.Fprintf(b, "and its %d %s", nr.LogicalVolumesDeleted, noun)
	if len(nr.ClaimsLeftWithoutLV) == 0 {
		b.WriteString("\n")
		return
	}
	b.WriteString("; these volumes would keep a PV without an LV:\n")
	for _, v := range nr.ClaimsLeftWithoutLV {
		fmt.Fprintf(b, "  %s\n", v)
	}
}

func yesNoUnknown(b *bool) string {
	if b == nil {
		return unknown
	}
	return yesNo(*b)
}
