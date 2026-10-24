package report

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/units"
)

// Pending reasons.
const (
	ReasonImmediate     = "Immediate"
	ReasonNoConsumer    = "NoConsumer"
	ReasonUnschedulable = "Unschedulable"
	ReasonProvisioning  = "Provisioning"
)

// SelectedNodeAnnotation is where the scheduler records the node a WaitForFirstConsumer claim goes to.
const SelectedNodeAnnotation = "volume.kubernetes.io/selected-node"

// PendingPVC explains one unbound TopoLVM claim.
type PendingPVC struct {
	Namespace    string    `json:"namespace"`
	PVC          string    `json:"pvc"`
	StorageClass string    `json:"storageClass"`
	DeviceClass  string    `json:"deviceClass"`
	SelectedNode string    `json:"selectedNode"`
	RequestBytes int64     `json:"requestBytes"`
	Created      time.Time `json:"createdAt"`
	// Since is when the claim started waiting for something TopoLVM controls: its creation for
	// Immediate binding, else the later of its creation and its first consumer pod.
	Since     time.Time `json:"pendingSince"`
	Reason    string    `json:"reason"`
	Detail    string    `json:"detail"`
	LastEvent string    `json:"lastEvent"`
}

func isWFFC(sc *storagev1.StorageClass) bool {
	return sc.VolumeBindingMode != nil && *sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer
}

// claimClass is the device class a StorageClass provisions on a node: its parameter, else the
// node's default class.
func claimClass(in *Input, sc *storagev1.StorageClass, n *nodeDefaults, node string) (string, bool) {
	if c := sc.Parameters[in.Driver.DeviceClassParameter()]; c != "" {
		return c, true
	}
	d, ok := n.byNode[node]
	return d, ok
}

// nodeDefaults caches the default class of every node.
type nodeDefaults struct {
	byNode map[string]string
	common string
}

func defaults(in *Input) *nodeDefaults {
	nd := &nodeDefaults{byNode: map[string]string{}}
	seen := map[string]bool{}
	for _, n := range in.Nodes {
		if d, ok := in.defaultOf(n); ok {
			nd.byNode[n.Name] = d
			seen[d] = true
		}
	}
	if len(seen) == 1 {
		for d := range seen {
			nd.common = d
		}
	} else {
		nd.common = DefaultPlaceholder
	}
	return nd
}

func consumers(in *Input) map[string][]*corev1.Pod {
	out := map[string][]*corev1.Pod{}
	for i := range in.Pods {
		p := &in.Pods[i]
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				key := p.Namespace + "/" + v.PersistentVolumeClaim.ClaimName
				out[key] = append(out[key], p)
			}
		}
	}
	return out
}

// BuildPending explains every Pending claim of a TopoLVM StorageClass.
func BuildPending(in Input, o Options) []PendingPVC {
	rows := []PendingPVC{}
	if in.PVCs == nil || in.StorageClasses == nil {
		return rows
	}
	scs := map[string]*storagev1.StorageClass{}
	for i := range in.StorageClasses {
		scs[in.StorageClasses[i].Name] = &in.StorageClasses[i]
	}
	nd := defaults(&in)
	users := consumers(&in)
	nodeFilter, classFilter := set(o.Nodes), set(o.DeviceClasses)
	for i := range in.PVCs {
		pvc := &in.PVCs[i]
		if pvc.Status.Phase != corev1.ClaimPending || pvc.Spec.StorageClassName == nil {
			continue
		}
		if o.Namespace != "" && pvc.Namespace != o.Namespace {
			continue
		}
		sc, ok := scs[*pvc.Spec.StorageClassName]
		if !ok || sc.Provisioner != in.Driver.Name {
			continue
		}
		row := PendingPVC{
			Namespace: pvc.Namespace, PVC: pvc.Name, StorageClass: sc.Name,
			SelectedNode: pvc.Annotations[SelectedNodeAnnotation], Created: pvc.CreationTimestamp.UTC(),
			DeviceClass: sc.Parameters[in.Driver.DeviceClassParameter()],
		}
		if row.DeviceClass == "" {
			row.DeviceClass = nd.common
		}
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			row.RequestBytes = q.Value()
		}
		pods := users[pvc.Namespace+"/"+pvc.Name]
		row.Since = row.Created
		if isWFFC(sc) && len(pods) > 0 {
			first := pods[0].CreationTimestamp.UTC()
			for _, p := range pods[1:] {
				if t := p.CreationTimestamp.UTC(); t.Before(first) {
					first = t
				}
			}
			if first.After(row.Since) {
				row.Since = first
			}
		}
		switch {
		case !isWFFC(sc):
			row.Reason = ReasonImmediate
			row.Detail = fmt.Sprintf("StorageClass %s uses Immediate binding; TopoLVM recommends WaitForFirstConsumer", sc.Name)
			row.LastEvent = lastEvent(&in, "PersistentVolumeClaim", pvc.Namespace, pvc.Name, "ProvisioningFailed")
		case len(pods) == 0:
			row.Reason, row.Detail = ReasonNoConsumer, "waiting for a pod that uses it"
		case row.SelectedNode == "":
			row.Reason = ReasonUnschedulable
			row.Detail = "scheduler has not picked a node yet"
			if maxFree, maxNode, class, ok := maxFreeFor(&in, sc, nd); ok && maxFree < row.RequestBytes {
				row.Detail = fmt.Sprintf("no node has %s free in class %s (max %s on %s)",
					units.FormatIEC(row.RequestBytes), class, units.FormatIEC(maxFree), maxNode)
			}
			for _, p := range pods {
				if e := lastEvent(&in, "Pod", p.Namespace, p.Name, "FailedScheduling"); e != "" {
					row.LastEvent = e
				}
			}
		default:
			row.Reason = ReasonProvisioning
			row.Detail = "provisioning on " + row.SelectedNode
			if class, ok := claimClass(&in, sc, nd, row.SelectedNode); ok {
				for _, n := range in.Nodes {
					if free, has := n.Capacity[class]; has && n.Name == row.SelectedNode && free < row.RequestBytes {
						row.Detail = fmt.Sprintf("%s has only %s free in class %s", n.Name, units.FormatIEC(free), class)
					}
				}
			}
			row.LastEvent = lastEvent(&in, "PersistentVolumeClaim", pvc.Namespace, pvc.Name, "ProvisioningFailed")
		}
		if !allowed(classFilter, row.DeviceClass) || (nodeFilter != nil && !nodeFilter[row.SelectedNode]) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].PVC < rows[j].PVC
	})
	return rows
}

// maxFreeFor is the largest free space any node offers in the class a StorageClass provisions.
func maxFreeFor(in *Input, sc *storagev1.StorageClass, nd *nodeDefaults) (maxFree int64, node, class string, found bool) {
	for _, n := range in.Nodes {
		c, ok := claimClass(in, sc, nd, n.Name)
		if !ok {
			continue
		}
		if free, has := n.Capacity[c]; has && (!found || free > maxFree) {
			maxFree, node, class, found = free, n.Name, c, true
		}
	}
	return maxFree, node, class, found
}
