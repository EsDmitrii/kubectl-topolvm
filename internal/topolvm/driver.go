// Package topolvm holds the TopoLVM domain types and how they are read from Kubernetes objects.
package topolvm

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Driver is a TopoLVM CSI driver name. Every other name TopoLVM uses (CRD group, annotation
// prefix, topology key, finalizers) derives from it, so legacy installations need no extra knobs.
type Driver struct{ Name string }

var (
	// Current is the driver name since TopoLVM 0.16.
	Current = Driver{Name: "topolvm.io"}
	// Legacy is the pre-0.16 name, still supported through the chart's useLegacy value.
	Legacy = Driver{Name: "topolvm.cybozu.com"}
)

// DefaultAlias is the capacity annotation suffix topolvm-node writes for the default device
// class next to the class's own key. It is never a class of its own.
const DefaultAlias = "00default"

// ParseDriver accepts only the two TopoLVM driver names.
func ParseDriver(s string) (Driver, error) {
	switch s {
	case Current.Name:
		return Current, nil
	case Legacy.Name:
		return Legacy, nil
	default:
		return Driver{}, fmt.Errorf("unknown driver %q: want %s or %s", s, Current.Name, Legacy.Name)
	}
}

// GVR is the LogicalVolume resource of this driver.
func (d Driver) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: d.Name, Version: "v1", Resource: "logicalvolumes"}
}

// CapacityPrefix is the node annotation prefix for free bytes per device class.
func (d Driver) CapacityPrefix() string { return "capacity." + d.Name + "/" }

// TopologyKey is the node label kubelet sets for nodes registered with the driver.
func (d Driver) TopologyKey() string { return "topology." + d.Name + "/node" }

// PendingDeletionKey is the LogicalVolume annotation the controller sets when the node is gone.
func (d Driver) PendingDeletionKey() string { return d.Name + "/pendingdeletion" }

// ResizeRequestedAtKey is the LogicalVolume annotation stamped when an expansion is requested.
func (d Driver) ResizeRequestedAtKey() string { return d.Name + "/resize-requested-at" }

// LVFinalizer is the LogicalVolume finalizer topolvm-node removes after lvremove.
func (d Driver) LVFinalizer() string { return d.Name + "/logicalvolume" }

// NodeFinalizer is the node finalizer that makes the controller clean up on node deletion.
func (d Driver) NodeFinalizer() string { return d.Name + "/node" }

// DeviceClassParameter is the StorageClass parameter that selects a device class.
func (d Driver) DeviceClassParameter() string { return d.Name + "/device-class" }
