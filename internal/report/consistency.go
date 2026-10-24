package report

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
	"github.com/EsDmitrii/kubectl-topolvm/internal/units"
)

// mismatchGrace holds back capacity-mismatch and capacity-missing while the provisioner may still
// be catching up: it polls capacity once a minute, and topolvm-node rewrites the annotation on every
// LV change.
const mismatchGrace = 2 * time.Minute

// mismatchTolerance absorbs CSIStorageCapacity being rounded to whole mebibytes.
const mismatchTolerance = 1 << 20

func init() {
	CheckIDs = append(CheckIDs,
		"storage-capacity-disabled", "immediate-binding", "no-default-class", "capacity-missing", "capacity-mismatch",
		"stale-class-annotation", "node-without-capacity", "topolvm-node-not-ready", "pvc-pending", "pvc-resize-stuck")
}

// extended runs the checks over capacity tracking, node registration and claims. Each needs data
// that commands other than check do not collect; missing data skips the check silently, because
// the collector already warned about what it could not read.
func (c *checker) extended() {
	in := c.in
	d := in.Driver
	tracking := false
	if in.CSIDriver != nil {
		tracking = in.CSIDriver.Spec.StorageCapacity != nil && *in.CSIDriver.Spec.StorageCapacity
		if !tracking && !in.SchedulerExtender {
			c.add("storage-capacity-disabled", "csidriver/"+d.Name, "",
				"scheduler ignores TopoLVM capacity: CSIDriver storageCapacity is not true")
		}
	}

	var ours []*storagev1.StorageClass
	for i := range in.StorageClasses {
		sc := &in.StorageClasses[i]
		if sc.Provisioner != d.Name {
			continue
		}
		ours = append(ours, sc)
		if !isWFFC(sc) {
			mode := string(storagev1.VolumeBindingImmediate)
			if sc.VolumeBindingMode != nil {
				mode = string(*sc.VolumeBindingMode)
			}
			c.about(sc.Parameters[d.DeviceClassParameter()], "").add("immediate-binding", "storageclass/"+sc.Name, "", nil, nil,
				fmt.Sprintf("volumeBindingMode %s; TopoLVM recommends WaitForFirstConsumer", mode))
		}
	}

	c.noDefaultClass(ours)

	if tracking && in.StorageClasses != nil && in.Capacities != nil {
		c.capacityTracking(ours)
	}

	if in.Classes != nil || in.NodeClasses != nil {
		for _, n := range in.Nodes {
			classes := in.classesOf(n.Name)
			if classes == nil {
				continue
			}
			for _, class := range n.NamedClasses() {
				if _, ok := classes[class]; !ok {
					c.about(class, "").add("stale-class-annotation", "node/"+n.Name, n.Name, nil, nil,
						fmt.Sprintf("class %s is not in the lvmd config; the annotation may be frozen", class))
				}
			}
		}
	}

	if in.CSINodes != nil {
		capacity := map[string]int{}
		for _, n := range in.Nodes {
			capacity[n.Name] = len(n.Capacity)
		}
		for i := range in.CSINodes {
			cn := &in.CSINodes[i]
			for _, drv := range cn.Spec.Drivers {
				if drv.Name == d.Name && capacity[cn.Name] == 0 {
					c.add("node-without-capacity", "node/"+cn.Name, cn.Name,
						fmt.Sprintf("registered with %s but has no capacity annotations", d.Name))
				}
			}
		}
	}

	if in.TopoLVMPods != nil {
		ready := map[string]bool{}
		for i := range in.TopoLVMPods {
			p := &in.TopoLVMPods[i]
			if podReady(p) {
				ready[p.Spec.NodeName] = true
			}
		}
		for _, n := range in.Nodes {
			if !ready[n.Name] {
				c.add("topolvm-node-not-ready", "node/"+n.Name, n.Name, "no ready topolvm-node pod")
			}
		}
	}

	for _, p := range BuildPending(*in, Options{}) {
		if p.Reason == ReasonNoConsumer {
			continue
		}
		if a := c.age(p.Since); a >= c.minAge {
			c.about(p.DeviceClass, p.Namespace).add("pvc-pending", "persistentvolumeclaim/"+p.Namespace+"/"+p.PVC, p.SelectedNode, new(p.RequestBytes), &a,
				p.Reason+": "+p.Detail)
		}
	}

	c.resizeStuck()
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// capacityTracking compares CSIStorageCapacity objects with the node annotations they derive from.
func (c *checker) capacityTracking(scs []*storagev1.StorageClass) {
	in := c.in
	key := in.Driver.TopologyKey()
	byNodeSC := map[string]map[string]*storagev1.CSIStorageCapacity{}
	for i := range in.Capacities {
		csc := &in.Capacities[i]
		if csc.NodeTopology == nil {
			continue
		}
		node := csc.NodeTopology.MatchLabels[key]
		if byNodeSC[node] == nil {
			byNodeSC[node] = map[string]*storagev1.CSIStorageCapacity{}
		}
		// The scheduler accepts a node when any matching object has room: keep the largest.
		if prev := byNodeSC[node][csc.StorageClassName]; prev == nil || larger(csc, prev) {
			byNodeSC[node][csc.StorageClassName] = csc
		}
	}
	recent := map[string]bool{}
	for i := range in.LVs {
		lv := &in.LVs[i]
		if c.age(lv.Created) < mismatchGrace {
			recent[lv.NodeName] = true
		}
		if t, err := parseGoTime(lv.Annotations[in.Driver.ResizeRequestedAtKey()]); err == nil && c.age(t) < mismatchGrace {
			recent[lv.NodeName] = true
		}
	}
	registered := map[string]time.Time{}
	for i := range in.CSINodes {
		registered[in.CSINodes[i].Name] = in.CSINodes[i].CreationTimestamp.Time
	}
	nd := defaults(in)
	sort.Slice(scs, func(i, j int) bool { return scs[i].Name < scs[j].Name })
	for _, n := range in.Nodes {
		for _, sc := range scs {
			if !isWFFC(sc) {
				continue
			}
			class, ok := claimClass(in, sc, nd, n.Name)
			if !ok {
				continue
			}
			free, ok := n.Capacity[class]
			if !ok {
				continue
			}
			csc := byNodeSC[n.Name][sc.Name]
			switch {
			case csc == nil:
				if c.age(sc.CreationTimestamp.Time) < mismatchGrace || c.age(registered[n.Name]) < mismatchGrace {
					continue
				}
				c.about(class, "").add("capacity-missing", "node/"+n.Name, n.Name, nil, nil,
					fmt.Sprintf("no CSIStorageCapacity for storageclass %s: the scheduler will skip this node", sc.Name))
			case csc.Capacity != nil && !recent[n.Name]:
				// Only an overstated capacity is a problem: right after a deletion the annotation
				// grows before the next poll, which understates it for a minute and is harmless.
				got := csc.Capacity.Value()
				if got-free > mismatchTolerance {
					c.about(class, "").add("capacity-mismatch", "node/"+n.Name, n.Name, nil, nil,
						fmt.Sprintf("CSIStorageCapacity %s says %s, node annotation says %s: the scheduler may pick this node for volumes that do not fit",
							csc.Name, units.FormatIEC(got), units.FormatIEC(free)))
				}
			}
		}
	}
}

// resizeStuck finds TopoLVM claims whose requested size stays above their capacity.
func (c *checker) resizeStuck() {
	in := c.in
	if in.PVCs == nil {
		return
	}
	pvByName := map[string]*corev1.PersistentVolume{}
	for i := range in.PVs {
		pvByName[in.PVs[i].Name] = &in.PVs[i]
	}
	users := consumers(in)
	for i := range in.PVCs {
		pvc := &in.PVCs[i]
		pv := pvByName[pvc.Spec.VolumeName]
		if pv == nil || pv.Spec.CSI == nil || pv.Spec.CSI.Driver != in.Driver.Name {
			continue
		}
		want, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		have, ok2 := pvc.Status.Capacity[corev1.ResourceStorage]
		if !ok || !ok2 || want.Cmp(have) <= 0 {
			continue
		}
		lv := findLV(in, pv.Spec.CSI.VolumeHandle)
		// Without a condition yet the resize is as old as TopoLVM's request annotation.
		since := pvc.CreationTimestamp.Time
		if lv != nil {
			if t, err := parseGoTime(lv.Annotations[in.Driver.ResizeRequestedAtKey()]); err == nil {
				since = t
			}
		}
		fsPending := false
		for _, cond := range pvc.Status.Conditions {
			if cond.Type == corev1.PersistentVolumeClaimResizing || cond.Type == corev1.PersistentVolumeClaimFileSystemResizePending {
				since = cond.LastTransitionTime.Time
				fsPending = cond.Type == corev1.PersistentVolumeClaimFileSystemResizePending
			}
		}
		// The LV is expanded already; kubelet grows the filesystem when a pod mounts the claim.
		if fsPending && lv != nil && lv.UsedBytes() >= want.Value() && in.Pods != nil && len(users[pvc.Namespace+"/"+pvc.Name]) == 0 {
			continue
		}
		a := c.age(since)
		if a < c.minAge {
			continue
		}
		node, class := "", ""
		if lv != nil {
			node, class = lv.NodeName, classResolver(in)(lv)
		}
		detail := fmt.Sprintf("requested %s, capacity %s", units.FormatIEC(want.Value()), units.FormatIEC(have.Value()))
		if msg := lastEvent(in, "PersistentVolumeClaim", pvc.Namespace, pvc.Name, "VolumeResizeFailed"); msg != "" {
			detail += "; last VolumeResizeFailed: " + msg
		}
		c.about(class, pvc.Namespace).add("pvc-resize-stuck", "persistentvolumeclaim/"+pvc.Namespace+"/"+pvc.Name, node, new(want.Value()), &a, detail)
	}
}

// noDefaultClass flags StorageClasses that rely on the default device class when no node has one:
// lvmd rejects their volumes. It fires only when every node proves it has none, never when the
// default merely cannot be told.
func (c *checker) noDefaultClass(ours []*storagev1.StorageClass) {
	if len(c.in.Nodes) == 0 {
		return
	}
	for _, n := range c.in.Nodes {
		if !noDefault(n, c.in.classesOf(n.Name)) {
			return
		}
	}
	for _, sc := range ours {
		if sc.Parameters[c.in.Driver.DeviceClassParameter()] == "" {
			c.add("no-default-class", "storageclass/"+sc.Name, "",
				"no device-class parameter and no node has a default device class: its claims cannot be provisioned")
		}
	}
}

func larger(a, b *storagev1.CSIStorageCapacity) bool {
	return a.Capacity != nil && (b.Capacity == nil || a.Capacity.Cmp(*b.Capacity) > 0)
}

// noDefault proves a node has no default class: its lvmd config marks none and no 00default above
// 0 says otherwise (lvmd fills it only from a default class), or, without a config, the alias is 0
// while no class is full (a full default class would also read 0).
func noDefault(n topolvm.Node, classes map[string]topolvm.DeviceClass) bool {
	alias, ok := n.Capacity[topolvm.DefaultAlias]
	if len(classes) > 0 {
		for _, dc := range classes {
			if dc.Default {
				return false
			}
		}
		return !ok || alias == 0
	}
	if !ok || alias != 0 {
		return false
	}
	for _, c := range n.NamedClasses() {
		if n.Capacity[c] == 0 {
			return false
		}
	}
	return true
}
