// Package report turns collected TopoLVM state into pools, volumes and findings. Everything here is
// a pure function of Input, so the whole logic is tested on fixtures without a cluster.
package report

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// Input is everything a report is computed from. A nil slice or map means the data was not
// collected (not asked for, or forbidden), which is different from collected and empty.
type Input struct {
	Driver      topolvm.Driver
	Now         time.Time
	Nodes       []topolvm.Node
	AbsentNodes map[string]bool
	LVs         []topolvm.LogicalVolume
	// SkippedLVs are the keys of LogicalVolumes that exist but could not be read.
	SkippedLVs []string
	PVs        []corev1.PersistentVolume
	Classes    map[string]topolvm.DeviceClass
	// NodeClasses has the device classes of the lvmd config each node runs with; nodes missing
	// from it fall back to Classes.
	NodeClasses map[string]map[string]topolvm.DeviceClass
	Thin        []string
	Warnings    []string

	PVCs           []corev1.PersistentVolumeClaim
	Pods           []corev1.Pod
	Events         []corev1.Event
	StorageClasses []storagev1.StorageClass
	Capacities     []storagev1.CSIStorageCapacity
	CSINodes       []storagev1.CSINode
	CSIDriver      *storagev1.CSIDriver
	// SchedulerExtender is set when topolvm-scheduler runs in the TopoLVM namespace: it filters
	// nodes by the capacity annotations, and CSI storage capacity stays off.
	SchedulerExtender bool
	NodeStates        map[string]NodeState
	TopoLVMPods       []corev1.Pod
	// ConfigPods are the pods in the TopoLVM namespace that mount an lvmd ConfigMap; NodeClasses is
	// derived from them, and dump writes them so a fixture can derive it again.
	ConfigPods []corev1.Pod
	// LVMDConfigMaps are the lvmd ConfigMaps as read, so dump can write them back unchanged.
	LVMDConfigMaps []corev1.ConfigMap
	Metrics        map[string]map[string]ClassMetrics
	Usage          map[string]VolumeUsage
}

// NodeState is what scheduling and node commands need beyond the capacity annotations.
type NodeState struct {
	Ready         bool           `json:"ready"`
	Unschedulable bool           `json:"unschedulable"`
	HasFinalizer  bool           `json:"hasFinalizer"`
	Taints        []corev1.Taint `json:"taints"`
}

// ClassMetrics are topolvm-node gauges for one device class on one node.
type ClassMetrics struct {
	VGSizeBytes         *int64   `json:"volumeGroupSizeBytes"`
	VGAvailableBytes    *int64   `json:"volumeGroupAvailableBytes"`
	ThinPoolSizeBytes   *int64   `json:"thinPoolSizeBytes"`
	ThinDataPercent     *float64 `json:"thinDataPercent"`
	ThinMetadataPercent *float64 `json:"thinMetadataPercent"`
}

// VolumeUsage is kubelet's filesystem view of one claim.
type VolumeUsage struct {
	UsedBytes     *int64 `json:"usedBytes"`
	CapacityBytes *int64 `json:"capacityBytes"`
	InodesUsed    *int64 `json:"inodesUsed"`
	Inodes        *int64 `json:"inodes"`
}

// Options filter and tune a report.
type Options struct {
	Nodes, DeviceClasses []string
	Namespace            string
	MinAge               time.Duration
	Skip                 []string
	// Checks turns on findings; commands other than check leave it off so data they do not
	// collect does not produce warnings.
	Checks bool
	// SizeNotes adds the notes that explain pool sizes (thin overprovisioning, unknown class
	// types); only commands that print pool sizes want them.
	SizeNotes bool
}

// UnknownFilter names the first --node or --device-class value that matches nothing: no TopoLVM
// node or LogicalVolume for nodes, no node, LogicalVolume or lvmd config for classes.
func (in *Input) UnknownFilter(nodes, classes []string) error {
	knownNodes, knownClasses := map[string]bool{}, map[string]bool{}
	for _, n := range in.Nodes {
		knownNodes[n.Name] = true
		for _, c := range n.NamedClasses() {
			knownClasses[c] = true
		}
	}
	for i := range in.LVs {
		knownNodes[in.LVs[i].NodeName] = true
		knownClasses[in.LVs[i].DeviceClass] = true
	}
	for c := range in.Classes {
		knownClasses[c] = true
	}
	for _, cs := range in.NodeClasses {
		for c := range cs {
			knownClasses[c] = true
		}
	}
	for _, n := range nodes {
		if !knownNodes[n] {
			return fmt.Errorf("node %q is not a TopoLVM node", n)
		}
	}
	for _, c := range classes {
		if !knownClasses[c] {
			return fmt.Errorf("device class %q is not on any TopoLVM node", c)
		}
	}
	return nil
}

// defaultOf is defaultClass for a node of the input, with its volumes as evidence.
func (in *Input) defaultOf(n topolvm.Node) (string, bool) {
	return defaultClass(n, in.classesOf(n.Name), func() bool {
		for i := range in.LVs {
			if in.LVs[i].NodeName == n.Name && in.LVs[i].DeviceClass == "" {
				return true
			}
		}
		return false
	})
}

// classesOf is the lvmd config of one node, or the cluster-wide view when the node's is unknown.
func (in *Input) classesOf(node string) map[string]topolvm.DeviceClass {
	if c, ok := in.NodeClasses[node]; ok {
		return c
	}
	return in.Classes
}

func set(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// allowed reports whether name passes a filter set; a nil set lets everything through.
func allowed(filter map[string]bool, name string) bool { return filter == nil || filter[name] }
