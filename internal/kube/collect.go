package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// ErrNoNodes means no node carries the TopoLVM topology label or capacity annotations.
var ErrNoNodes = errors.New("no TopoLVM nodes found")

// RBACHint tells a user without enough rights what to ask for.
const RBACHint = "the plugin needs get/list on nodes, persistentvolumes, csidrivers and logicalvolumes " +
	"(see https://github.com/EsDmitrii/kubectl-topolvm#rbac)"

// pageSize bounds every list request; Continue tokens fetch the rest.
const pageSize = 500

var (
	nodesGVR          = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	lvmVolumeGroupGVR = schema.GroupVersionResource{Group: "lvm.topolvm.io", Version: "v1alpha1", Resource: "lvmvolumegroups"}
)

// PVMode says whether a command needs PersistentVolumes.
type PVMode int

// PersistentVolume modes.
const (
	PVSkip PVMode = iota
	PVOptional
	PVRequired
)

// Needs is a set of optional data a command asks for.
type Needs uint

// Optional data. NeedStorage covers StorageClasses, CSIStorageCapacities, CSINodes and the CSIDriver.
const (
	NeedPVCs Needs = 1 << iota
	NeedPods
	NeedEvents
	NeedStorage
	NeedNodeStates
	NeedTopoLVMPods
	NeedMetrics
	NeedUsage
)

// CollectOptions say what to read.
type CollectOptions struct {
	Driver           topolvm.Driver
	TopoLVMNamespace string
	PVs              PVMode
	Needs            Needs
	Thin             []string
	Now              time.Time
}

func (o CollectOptions) need(n Needs) bool { return o.Needs&n != 0 }

// listAll follows Continue tokens until the list is complete. When a token expires half way it
// starts over with one unpaginated list, as client-go's pager does.
func listAll[T any](ctx context.Context, opts metav1.ListOptions, list func(context.Context, metav1.ListOptions) ([]T, string, error)) ([]T, error) {
	opts.Limit = pageSize
	var out []T
	for {
		items, cont, err := list(ctx, opts)
		if err != nil {
			if apierrors.IsResourceExpired(err) && opts.Continue != "" {
				opts.Continue, opts.Limit, out = "", 0, nil
				continue
			}
			return nil, err
		}
		out = append(out, items...)
		if cont == "" {
			return out, nil
		}
		opts.Continue = cont
	}
}

// collected marks a list as read: nil means not collected, so an empty result is an empty slice.
func collected[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func (c *Clients) listNodeMeta(ctx context.Context, opts metav1.ListOptions) ([]metav1.PartialObjectMetadata, error) {
	return listAll(ctx, opts, func(ctx context.Context, o metav1.ListOptions) ([]metav1.PartialObjectMetadata, string, error) {
		l, err := c.Meta.Resource(nodesGVR).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
}

// discoverNodes finds TopoLVM nodes: labelled with the topology key by kubelet, or carrying capacity
// annotations. A node with annotations but no label lost its driver registration; it is kept with a
// warning, because the scheduler cannot place volumes there.
func (c *Clients) discoverNodes(ctx context.Context, d topolvm.Driver) ([]metav1.PartialObjectMetadata, []string, error) {
	all, err := c.listNodeMeta(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, rbacErr("nodes", err)
	}
	var nodes, unlabelled []metav1.PartialObjectMetadata
	for i := range all {
		if _, ok := all[i].Labels[d.TopologyKey()]; ok {
			nodes = append(nodes, all[i])
			continue
		}
		for k := range all[i].Annotations {
			if strings.HasPrefix(k, d.CapacityPrefix()) {
				unlabelled = append(unlabelled, all[i])
				break
			}
		}
	}
	var warnings []string
	if len(nodes) > 0 {
		for i := range unlabelled {
			warnings = append(warnings, fmt.Sprintf("node %s has TopoLVM capacity annotations but no %s label: the driver is not "+
				"registered there, the scheduler cannot place volumes on it", unlabelled[i].Name, d.TopologyKey()))
		}
	}
	nodes = append(nodes, unlabelled...)
	if len(nodes) == 0 {
		return nil, nil, fmt.Errorf("%w (no nodes labelled %s or annotated %s*)", ErrNoNodes, d.TopologyKey(), d.CapacityPrefix())
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, warnings, nil
}

func rbacErr(resource string, err error) error {
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("cannot list %s: forbidden; %s", resource, RBACHint)
	}
	return fmt.Errorf("list %s: %w", resource, err)
}

// listLVs reads every LogicalVolume. One that cannot be parsed is skipped with a warning rather than
// costing the whole view.
func (c *Clients) listLVs(ctx context.Context, d topolvm.Driver) (lvs []topolvm.LogicalVolume, skipped, warnings []string, err error) {
	resource := "logicalvolumes." + d.Name
	items, err := listAll(ctx, metav1.ListOptions{}, func(ctx context.Context, o metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
		l, lerr := c.Dynamic.Resource(d.GVR()).List(ctx, o)
		if lerr != nil {
			return nil, "", lerr
		}
		return l.Items, l.GetContinue(), nil
	})
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil, nil, fmt.Errorf("%s not found: is TopoLVM installed? (try --driver)", resource)
	case err != nil:
		return nil, nil, nil, rbacErr(resource, err)
	}
	lvs = make([]topolvm.LogicalVolume, 0, len(items))
	for i := range items {
		lv, perr := topolvm.LogicalVolumeFrom(&items[i])
		if perr != nil {
			warnings = append(warnings, perr.Error()+"; skipped")
			skipped = append(skipped, lv.Key())
			continue
		}
		lvs = append(lvs, lv)
	}
	return lvs, skipped, warnings, nil
}

func (c *Clients) listPVs(ctx context.Context) ([]corev1.PersistentVolume, error) {
	return listAll(ctx, metav1.ListOptions{}, func(ctx context.Context, o metav1.ListOptions) ([]corev1.PersistentVolume, string, error) {
		l, err := c.Core.CoreV1().PersistentVolumes().List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
}

// listConfigMaps returns the ConfigMaps of the namespace that carry lvmd.yaml. No label selector:
// a chart nameOverride changes the labels.
func (c *Clients) listConfigMaps(ctx context.Context, namespace string) ([]corev1.ConfigMap, error) {
	l, err := c.Core.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []corev1.ConfigMap
	for i := range l.Items {
		if _, ok := l.Items[i].Data["lvmd.yaml"]; ok {
			out = append(out, l.Items[i])
		}
	}
	return out, nil
}

// optionalWarning words a failed optional read: what failed and what the output lacks because of it.
func optionalWarning(verb, resource string, err error, consequence string) string {
	reason := err.Error()
	if apierrors.IsForbidden(err) {
		reason = "forbidden"
	}
	return fmt.Sprintf("cannot %s %s: %s; %s", verb, resource, reason, consequence)
}

// eventReasons are the only events the plugin reads: scheduling, provisioning, resize and delete
// failures.
var eventReasons = []string{"FailedScheduling", "ProvisioningFailed", "VolumeResizeFailed", "VolumeFailedDelete"}

func (c *Clients) listEvents(ctx context.Context) ([]corev1.Event, error) {
	seen := map[string]bool{}
	var out []corev1.Event
	for _, reason := range eventReasons {
		items, err := listAll(ctx, metav1.ListOptions{FieldSelector: "reason=" + reason}, func(ctx context.Context, o metav1.ListOptions) ([]corev1.Event, string, error) {
			l, err := c.Core.CoreV1().Events("").List(ctx, o)
			if err != nil {
				return nil, "", err
			}
			return l.Items, l.Continue, nil
		})
		if err != nil {
			return nil, err
		}
		for i := range items {
			key := string(items[i].UID)
			if key == "" {
				key = items[i].Namespace + "/" + items[i].Name
			}
			if !seen[key] {
				seen[key] = true
				out = append(out, items[i])
			}
		}
	}
	return out, nil
}

func (c *Clients) listPods(ctx context.Context, namespace, fieldSelector string) ([]corev1.Pod, error) {
	return listAll(ctx, metav1.ListOptions{FieldSelector: fieldSelector}, func(ctx context.Context, o metav1.ListOptions) ([]corev1.Pod, string, error) {
		l, err := c.Core.CoreV1().Pods(namespace).List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
}

func (c *Clients) listPVCs(ctx context.Context) ([]corev1.PersistentVolumeClaim, error) {
	return listAll(ctx, metav1.ListOptions{}, func(ctx context.Context, o metav1.ListOptions) ([]corev1.PersistentVolumeClaim, string, error) {
		l, err := c.Core.CoreV1().PersistentVolumeClaims("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
}

func hasContainer(p *corev1.Pod, name string) bool {
	for i := range p.Spec.Containers {
		if p.Spec.Containers[i].Name == name {
			return true
		}
	}
	return false
}

func nodeState(n *corev1.Node, d topolvm.Driver) report.NodeState {
	s := report.NodeState{Unschedulable: n.Spec.Unschedulable, Taints: n.Spec.Taints}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			s.Ready = c.Status == corev1.ConditionTrue
		}
	}
	for _, f := range n.Finalizers {
		s.HasFinalizer = s.HasFinalizer || f == d.NodeFinalizer()
	}
	return s
}

// result collects what concurrent reads return; each read writes its own fields under mu.
type result struct {
	mu       sync.Mutex
	in       *report.Input
	warnings []string
}

func (r *result) set(fn func(in *report.Input)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.in)
}

func (r *result) warn(w string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warnings = append(r.warnings, w)
}

// collectOptional runs every optional read o asks for. Failures become warnings.
func (c *Clients) collectOptional(ctx context.Context, o CollectOptions, res *result) {
	var wg sync.WaitGroup
	if o.need(NeedPVCs) {
		wg.Go(func() {
			items, err := c.listPVCs(ctx)
			if err != nil {
				res.warn(optionalWarning("list", "persistentvolumeclaims", err, "claim-based output is incomplete"))
				return
			}
			res.set(func(in *report.Input) { in.PVCs = collected(items) })
		})
	}
	if o.need(NeedPods) {
		wg.Go(func() {
			items, err := c.listPods(ctx, "", "status.phase!=Succeeded,status.phase!=Failed")
			if err != nil {
				res.warn(optionalWarning("list", "pods", err, "consumers and owners of volumes are unknown"))
				return
			}
			res.set(func(in *report.Input) { in.Pods = collected(items) })
		})
	}
	if o.need(NeedEvents) {
		wg.Go(func() {
			items, err := c.listEvents(ctx)
			if err != nil {
				res.warn(optionalWarning("list", "events", err, "last event messages are not shown"))
				return
			}
			res.set(func(in *report.Input) { in.Events = collected(items) })
		})
	}
	if o.need(NeedStorage) {
		wg.Go(func() {
			items, err := listAll(ctx, metav1.ListOptions{}, func(ctx context.Context, o metav1.ListOptions) ([]storagev1.StorageClass, string, error) {
				l, err := c.Core.StorageV1().StorageClasses().List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return l.Items, l.Continue, nil
			})
			if err != nil {
				res.warn(optionalWarning("list", "storageclasses", err, "StorageClass-based output is incomplete"))
				return
			}
			res.set(func(in *report.Input) { in.StorageClasses = collected(items) })
		})
		wg.Go(func() {
			items, err := listAll(ctx, metav1.ListOptions{}, func(ctx context.Context, o metav1.ListOptions) ([]storagev1.CSIStorageCapacity, string, error) {
				l, err := c.Core.StorageV1().CSIStorageCapacities("").List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return l.Items, l.Continue, nil
			})
			if err != nil {
				res.warn(optionalWarning("list", "csistoragecapacities", err, "capacity tracking checks are skipped"))
				return
			}
			res.set(func(in *report.Input) { in.Capacities = collected(items) })
		})
		wg.Go(func() {
			items, err := listAll(ctx, metav1.ListOptions{}, func(ctx context.Context, o metav1.ListOptions) ([]storagev1.CSINode, string, error) {
				l, err := c.Core.StorageV1().CSINodes().List(ctx, o)
				if err != nil {
					return nil, "", err
				}
				return l.Items, l.Continue, nil
			})
			if err != nil {
				res.warn(optionalWarning("list", "csinodes", err, "node registration checks are skipped"))
				return
			}
			res.set(func(in *report.Input) { in.CSINodes = collected(items) })
		})
		wg.Go(func() {
			d, err := c.Core.StorageV1().CSIDrivers().Get(ctx, o.Driver.Name, metav1.GetOptions{})
			if err != nil {
				res.warn(optionalWarning("get", "csidriver "+o.Driver.Name, err, "the storage capacity check is skipped"))
				return
			}
			res.set(func(in *report.Input) { in.CSIDriver = d })
		})
	}
	wg.Wait()
}

// topolvmPods sorts the pods of the TopoLVM namespace: those mounting an lvmd ConfigMap give the
// per-node device classes, those with a topolvm-node container serve readiness and metrics.
func topolvmPods(in *report.Input, o CollectOptions, pods []corev1.Pod, podsErr error, cms []corev1.ConfigMap) {
	for i := range pods {
		in.SchedulerExtender = in.SchedulerExtender || hasContainer(&pods[i], "topolvm-scheduler")
	}
	if podsErr == nil {
		lvmdConfigs := map[string]bool{}
		for _, cm := range cms {
			if _, ok := cm.Data["lvmd.yaml"]; ok {
				lvmdConfigs[cm.Name] = true
				in.LVMDConfigMaps = append(in.LVMDConfigMaps, cm)
			}
		}
		for i := range pods {
			for _, v := range pods[i].Spec.Volumes {
				if v.ConfigMap != nil && lvmdConfigs[v.ConfigMap.Name] {
					in.ConfigPods = append(in.ConfigPods, pods[i])
					break
				}
			}
		}
		in.NodeClasses = topolvm.NodeClasses(in.ConfigPods, cms)
		if len(pods) == 0 {
			in.Warnings = append(in.Warnings, fmt.Sprintf("no pods in namespace %s; pass --topolvm-namespace "+
				"(lvmd config, topolvm-node readiness and --metrics are skipped)", o.TopoLVMNamespace))
			return
		}
	}
	if !o.need(NeedTopoLVMPods) {
		return
	}
	if podsErr != nil {
		in.Warnings = append(in.Warnings, optionalWarning("list", "pods in "+o.TopoLVMNamespace, podsErr, "topolvm-node readiness is unknown"))
		return
	}
	var nodePods []corev1.Pod
	for i := range pods {
		if hasContainer(&pods[i], "topolvm-node") {
			nodePods = append(nodePods, pods[i])
		}
	}
	if len(nodePods) == 0 {
		in.Warnings = append(in.Warnings, fmt.Sprintf("no topolvm-node pods in namespace %s; pass --topolvm-namespace "+
			"(topolvm-node readiness and --metrics are skipped)", o.TopoLVMNamespace))
		return
	}
	in.TopoLVMPods = nodePods
}

// collectNodeStates reads readiness, cordon, taints and the TopoLVM finalizer of known nodes, page
// by page so that full Node objects of a large cluster are never all held at once.
func (c *Clients) collectNodeStates(ctx context.Context, o CollectOptions, in *report.Input) {
	wanted := map[string]bool{}
	for _, n := range in.Nodes {
		wanted[n.Name] = true
	}
	type named struct {
		name  string
		state report.NodeState
	}
	items, err := listAll(ctx, metav1.ListOptions{}, func(ctx context.Context, lo metav1.ListOptions) ([]named, string, error) {
		l, err := c.Core.CoreV1().Nodes().List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		var out []named
		for i := range l.Items {
			if wanted[l.Items[i].Name] {
				out = append(out, named{l.Items[i].Name, nodeState(&l.Items[i], o.Driver)})
			}
		}
		return out, l.Continue, nil
	})
	if err != nil {
		in.Warnings = append(in.Warnings, optionalWarning("list", "nodes", err, "node readiness and taints are unknown"))
		return
	}
	in.NodeStates = map[string]report.NodeState{}
	for _, it := range items {
		in.NodeStates[it.name] = it.state
	}
}

// classes reads device classes from the lvmd ConfigMaps, falling back to LVMS LVMVolumeGroups. The
// ConfigMaps come back too, for the per-node view. Without either the types are guessed, so a
// refused read is a warning; a missing LVMS API is not.
func (c *Clients) classes(ctx context.Context, namespace string) (classes map[string]topolvm.DeviceClass, warnings []string, cms []corev1.ConfigMap) {
	list, cmErr := c.listConfigMaps(ctx, namespace)
	if cmErr == nil {
		cms = list
		if classes, warnings = topolvm.ClassesFromConfigMaps(cms); classes != nil {
			return classes, warnings, cms
		}
	}
	l, err := c.Dynamic.Resource(lvmVolumeGroupGVR).List(ctx, metav1.ListOptions{})
	if err == nil {
		if lvms := topolvm.ClassesFromLVMVolumeGroups(l.Items); lvms != nil {
			return lvms, warnings, cms
		}
	}
	switch {
	case cmErr != nil:
		warnings = append(warnings, optionalWarning("list", "configmaps in "+namespace, cmErr,
			"device class types and volume groups are guessed (kubectl topolvm rbac prints the Role)"))
	case err != nil && !apierrors.IsNotFound(err):
		warnings = append(warnings, optionalWarning("list", "lvmvolumegroups.lvm.topolvm.io", err,
			"on Red Hat LVMS device class types are guessed (kubectl topolvm rbac --with lvms prints the rule)"))
	}
	return nil, warnings, cms
}

// Collect reads what o asks for. Required data that cannot be read is an error; optional data
// becomes a warning and a nil field in Input.
func Collect(ctx context.Context, c *Clients, o CollectOptions) (report.Input, error) {
	if o.need(NeedMetrics) {
		o.Needs |= NeedTopoLVMPods
	}
	in := report.Input{Driver: o.Driver, Now: o.Now, Thin: o.Thin, AbsentNodes: map[string]bool{}}
	res := &result{in: &in}

	var (
		wg             sync.WaitGroup
		nodes          []metav1.PartialObjectMetadata
		nodeWarnings   []string
		lvs            []topolvm.LogicalVolume
		lvWarnings     []string
		skippedLVs     []string
		pvs            []corev1.PersistentVolume
		classes        map[string]topolvm.DeviceClass
		classWarnings  []string
		cms            []corev1.ConfigMap
		nsPods         []corev1.Pod
		nsPodsErr      error
		nodeErr, lvErr error
		pvErr          error
	)
	wg.Go(func() { nodes, nodeWarnings, nodeErr = c.discoverNodes(ctx, o.Driver) })
	wg.Go(func() { lvs, skippedLVs, lvWarnings, lvErr = c.listLVs(ctx, o.Driver) })
	if o.PVs != PVSkip {
		wg.Go(func() { pvs, pvErr = c.listPVs(ctx) })
	}
	wg.Go(func() { classes, classWarnings, cms = c.classes(ctx, o.TopoLVMNamespace) })
	wg.Go(func() { nsPods, nsPodsErr = c.listPods(ctx, o.TopoLVMNamespace, "") })
	wg.Go(func() { c.collectOptional(ctx, o, res) })
	wg.Wait()

	if nodeErr != nil {
		return report.Input{}, nodeErr
	}
	if lvErr != nil {
		return report.Input{}, lvErr
	}
	in.LVs, in.SkippedLVs = lvs, skippedLVs
	in.Warnings = append(in.Warnings, nodeWarnings...)
	in.Warnings = append(in.Warnings, lvWarnings...)
	switch {
	case o.PVs == PVSkip:
	case pvErr == nil:
		in.PVs = collected(pvs)
	case apierrors.IsForbidden(pvErr) && o.PVs == PVOptional:
		in.Warnings = append(in.Warnings, "cannot list persistentvolumes: forbidden; PVC columns stay empty")
	default:
		return report.Input{}, rbacErr("persistentvolumes", pvErr)
	}
	in.Classes = classes
	in.Warnings = append(in.Warnings, classWarnings...)
	topolvmPods(&in, o, nsPods, nsPodsErr, cms)
	sort.Strings(res.warnings)
	in.Warnings = append(in.Warnings, res.warnings...)

	known := map[string]bool{}
	for i := range nodes {
		in.Nodes = append(in.Nodes, topolvm.NodeFrom(nodes[i].Name, nodes[i].Annotations, o.Driver))
		known[nodes[i].Name] = true
	}
	for i := range in.LVs {
		name := in.LVs[i].NodeName
		if known[name] || in.AbsentNodes[name] || name == "" {
			continue
		}
		n, err := c.Meta.Resource(nodesGVR).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			in.AbsentNodes[name] = true
		case err != nil:
			in.Warnings = append(in.Warnings, fmt.Sprintf("cannot get node %s: %v", name, err))
			known[name] = true
		default:
			in.Nodes = append(in.Nodes, topolvm.NodeFrom(n.Name, n.Annotations, o.Driver))
			known[name] = true
		}
	}
	sort.Slice(in.Nodes, func(i, j int) bool { return in.Nodes[i].Name < in.Nodes[j].Name })
	if o.need(NeedNodeStates) {
		c.collectNodeStates(ctx, o, &in)
	}
	if o.need(NeedMetrics) && in.TopoLVMPods != nil && c.Raw != nil {
		c.collectMetrics(ctx, &in)
	}
	if o.need(NeedUsage) && c.Raw != nil {
		c.collectUsage(ctx, &in)
	}
	return in, nil
}
