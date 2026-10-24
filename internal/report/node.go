package report

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// NodeReport is everything TopoLVM keeps on one node.
type NodeReport struct {
	Node                string       `json:"node"`
	State               *NodeState   `json:"state"`
	Pools               []Pool       `json:"pools"`
	Volumes             []NodeVolume `json:"volumes"`
	DeletedOnNodeDelete []string     `json:"deletedOnNodeDelete"`
	// LogicalVolumesDeleted counts the LogicalVolumes the controller deletes with the Node.
	LogicalVolumesDeleted int `json:"logicalVolumesDeletedOnNodeDelete"`
	// ClaimsLeftWithoutLV are the volumes, by claim or else by PV, whose PersistentVolume outlives
	// the Node without an LV.
	ClaimsLeftWithoutLV []string    `json:"claimsLeftWithoutLV"`
	Evacuation          []Placement `json:"evacuation"`
}

// NodeVolume is a volume on the node with the workload that mounts it.
type NodeVolume struct {
	Volume
	Owner string   `json:"owner"`
	Pods  []string `json:"pods"`
	// SingleCopy is unknown (nil) when pods were not collected.
	SingleCopy *bool `json:"singleCopy"`
}

// Placement is where a volume of the node could go; an empty Target means nowhere.
type Placement struct {
	Volume      string `json:"volume"`
	DeviceClass string `json:"deviceClass"`
	SizeBytes   int64  `json:"sizeBytes"`
	Target      string `json:"target"`
}

func controllerOf(p *corev1.Pod) string {
	if ref := metav1.GetControllerOf(p); ref != nil {
		return ref.Kind + "/" + ref.Name
	}
	return ""
}

// topolvmClaims maps namespace/claim of every TopoLVM PersistentVolume to the node of its LV.
func topolvmClaims(in *Input) map[string]string {
	lvNode := map[string]string{}
	for i := range in.LVs {
		lvNode[in.LVs[i].Key()] = in.LVs[i].NodeName
	}
	out := map[string]string{}
	for i := range in.PVs {
		pv := &in.PVs[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != in.Driver.Name || pv.Spec.ClaimRef == nil {
			continue
		}
		out[pv.Spec.ClaimRef.Namespace+"/"+pv.Spec.ClaimRef.Name] = lvNode[pv.Spec.CSI.VolumeHandle]
	}
	return out
}

// BuildNode reports one node: its pools, the volumes and their workloads, what TopoLVM deletes
// with the Node object (node_controller.go removes every claim of a TopoLVM StorageClass whose
// selected-node is this node, whatever the reclaim policy, then every LogicalVolume of the node),
// and a first-fit-decreasing placement of its volumes on the other nodes.
func BuildNode(in Input, name string) (NodeReport, error) {
	if !slices.ContainsFunc(in.Nodes, func(n topolvm.Node) bool { return n.Name == name }) {
		return NodeReport{}, fmt.Errorf("node %q is not a TopoLVM node", name)
	}
	nr := NodeReport{Node: name, Volumes: []NodeVolume{}, Evacuation: []Placement{}}
	if s, ok := in.NodeStates[name]; ok {
		nr.State = &s
	}
	nr.Pools, _ = buildPools(in, Options{Nodes: []string{name}})

	claimNode := topolvmClaims(&in)
	type podRef struct{ name, owner, node string }
	podsByClaim := map[string][]podRef{}
	for i := range in.Pods {
		p := &in.Pods[i]
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				key := p.Namespace + "/" + v.PersistentVolumeClaim.ClaimName
				podsByClaim[key] = append(podsByClaim[key], podRef{p.Name, controllerOf(p), p.Spec.NodeName})
			}
		}
	}
	// copiesElsewhere[owner] is true when a pod of that owner mounts a TopoLVM claim on another node.
	copiesElsewhere := map[string]bool{}
	for claim, pods := range podsByClaim {
		node, ok := claimNode[claim]
		if !ok || node == name {
			continue
		}
		for _, p := range pods {
			if p.owner != "" {
				ns, _, _ := strings.Cut(claim, "/")
				copiesElsewhere[ns+"/"+p.owner] = true
			}
		}
	}

	for _, v := range buildVolumes(in, Options{Nodes: []string{name}}) {
		nv := NodeVolume{Volume: v, Pods: []string{}}
		if v.Namespace != nil && v.PVC != nil {
			key := *v.Namespace + "/" + *v.PVC
			for _, p := range podsByClaim[key] {
				nv.Pods = append(nv.Pods, p.name)
				if nv.Owner == "" {
					nv.Owner = p.owner
				}
			}
		}
		if in.Pods != nil {
			nv.SingleCopy = new(nv.Owner != "" && v.Namespace != nil && !copiesElsewhere[*v.Namespace+"/"+nv.Owner])
		}
		sort.Strings(nv.Pods)
		nr.Volumes = append(nr.Volumes, nv)
	}

	switch {
	case nr.State != nil && !nr.State.HasFinalizer:
		// Without its finalizer on the Node the controller never sees the deletion.
		nr.DeletedOnNodeDelete, nr.ClaimsLeftWithoutLV = []string{}, []string{}
	default:
		for i := range in.LVs {
			if in.LVs[i].NodeName == name {
				nr.LogicalVolumesDeleted++
			}
		}
		if in.StorageClasses != nil && in.PVCs != nil {
			nr.DeletedOnNodeDelete, nr.ClaimsLeftWithoutLV = nodeDelete(&in, name, nr.Volumes)
		}
	}

	nr.Evacuation = evacuate(&in, name, nr.Volumes)
	return nr, nil
}

// nodeDelete lists the claims the controller deletes with the Node, and the volumes whose
// PersistentVolume stays without an LV: all but deleted claims whose PV goes under Delete.
func nodeDelete(in *Input, name string, volumes []NodeVolume) (deleted, left []string) {
	ours := map[string]bool{}
	for i := range in.StorageClasses {
		if in.StorageClasses[i].Provisioner == in.Driver.Name {
			ours[in.StorageClasses[i].Name] = true
		}
	}
	deleted = []string{}
	for i := range in.PVCs {
		pvc := &in.PVCs[i]
		if pvc.Annotations[SelectedNodeAnnotation] == name && pvc.Spec.StorageClassName != nil && ours[*pvc.Spec.StorageClassName] {
			deleted = append(deleted, pvc.Namespace+"/"+pvc.Name)
		}
	}
	sort.Strings(deleted)
	left = []string{}
	for _, v := range volumes {
		if v.Kind == KindSnapshot || v.PV == nil {
			continue
		}
		label := "persistentvolume/" + *v.PV
		if v.Namespace != nil && v.PVC != nil {
			label = *v.Namespace + "/" + *v.PVC
		}
		if slices.Contains(deleted, label) && v.ReclaimPolicy != nil && *v.ReclaimPolicy == string(corev1.PersistentVolumeReclaimDelete) {
			continue
		}
		left = append(left, label)
	}
	sort.Strings(left)
	return deleted, left
}

// evacuate places the node's volumes, largest first, on the first other Ready, schedulable node
// of the same class with room left after the volumes already placed.
func evacuate(in *Input, from string, volumes []NodeVolume) []Placement {
	free := map[string]map[string]int64{}
	var order []string
	for _, n := range in.Nodes {
		if n.Name == from || !schedulable(in, n.Name) {
			continue
		}
		free[n.Name] = maps.Clone(n.Capacity)
		order = append(order, n.Name)
	}
	sorted := make([]NodeVolume, 0, len(volumes))
	for _, v := range volumes {
		if v.Kind != KindSnapshot && v.Status != StatusDeleting {
			sorted = append(sorted, v)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SizeBytes > sorted[j].SizeBytes })
	out := make([]Placement, 0, len(sorted))
	for _, v := range sorted {
		label := v.LV
		if v.Namespace != nil && v.PVC != nil {
			label = *v.Namespace + "/" + *v.PVC
		}
		p := Placement{Volume: label, DeviceClass: v.DeviceClass, SizeBytes: v.SizeBytes}
		for _, n := range order {
			if room, ok := free[n][v.DeviceClass]; ok && room >= v.SizeBytes {
				p.Target = n
				free[n][v.DeviceClass] = room - v.SizeBytes
				break
			}
		}
		out = append(out, p)
	}
	return out
}
