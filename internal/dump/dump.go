// Package dump writes collected state in the fixture layout the tests read, so a bug report is a
// ready-made test case. Objects are always cut down to the fields the plugin reads, which keeps pod
// environments and images out; anonymizing also renames nodes, namespaces, claims, pods and owners.
package dump

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/EsDmitrii/kubectl-topolvm/internal/fixture"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// generated names carry no information and are kept as they are.
var generated = regexp.MustCompile(`^(pvc|snapshot)-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Kinds of renamed names and the prefix of their replacements.
var kinds = map[string]string{
	"node": "node", "ns": "ns", "claim": "claim", "pod": "pod", "owner": "owner", "pv": "pv", "lv": "lv", "event": "event",
	"sc": "sc", "finalizer": "finalizer", "taint": "taint",
}

// wellKnown are the finalizers and taint keys of Kubernetes itself, which say nothing about a site.
func wellKnown(key, driver string) bool {
	return strings.HasPrefix(key, driver+"/") || strings.Contains(key, "kubernetes.io/") || strings.Contains(key, "k8s.io/")
}

var ipv4 = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)

type anonymizer struct {
	on     bool
	seen   map[string]map[string]bool
	names  map[string]map[string]string
	byText map[string]string
}

func newAnonymizer(on bool) *anonymizer {
	a := &anonymizer{on: on, seen: map[string]map[string]bool{}, names: map[string]map[string]string{}}
	for k := range kinds {
		a.seen[k] = map[string]bool{}
	}
	return a
}

func (a *anonymizer) note(kind string, names ...string) {
	for _, n := range names {
		if n == "" || ((kind == "pv" || kind == "lv") && generated.MatchString(n)) {
			continue
		}
		a.seen[kind][n] = true
	}
}

// freeze numbers every noted name in sorted order, so the same cluster always dumps the same way.
func (a *anonymizer) freeze() {
	a.byText = map[string]string{}
	for kind, set := range a.seen {
		list := make([]string, 0, len(set))
		for n := range set {
			list = append(list, n)
		}
		sort.Strings(list)
		a.names[kind] = map[string]string{}
		for i, n := range list {
			a.names[kind][n] = fmt.Sprintf("%s-%d", kinds[kind], i+1)
			if kind != "event" {
				a.byText[n] = a.names[kind][n]
			}
		}
	}
}

func (a *anonymizer) name(kind, n string) string {
	if !a.on || n == "" {
		return n
	}
	if m, ok := a.names[kind][n]; ok {
		return m
	}
	return n
}

func isNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

// message replaces every known name standing as a whole word in free text, in one pass over the
// original, so a replacement is never replaced again. A dot before a name joins it to the word
// (a.node-1 stays), a dot after it ends a sentence (node-1. is replaced).
func (a *anonymizer) message(s string) string {
	if !a.on || len(a.byText) == 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if i == 0 || (!isNameByte(s[i-1]) && s[i-1] != '.') {
			if t, ok := a.wordAt(s, i); ok {
				b.WriteString(a.byText[t])
				i += len(t)
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return ipv4.ReplaceAllString(b.String(), "x.x.x.x")
}

// wordAt finds the longest known name at i. Names are name bytes and dots, and one ends where the
// text has neither or a dot, so only those prefixes of the run at i are looked up.
func (a *anonymizer) wordAt(s string, i int) (string, bool) {
	j := i
	for j < len(s) && (isNameByte(s[j]) || s[j] == '.') {
		j++
	}
	for k := j; k > i; k-- {
		if k < j && s[k] != '.' {
			continue
		}
		if _, ok := a.byText[s[i:k]]; ok {
			return s[i:k], true
		}
	}
	return "", false
}

func (a *anonymizer) collect(in *report.Input) {
	for _, n := range in.Nodes {
		a.note("node", n.Name)
	}
	for n := range in.AbsentNodes {
		a.note("node", n)
	}
	for n := range in.NodeStates {
		a.note("node", n)
	}
	for i := range in.LVs {
		a.note("node", in.LVs[i].NodeName)
		a.note("lv", in.LVs[i].Name, in.LVs[i].Source)
		for _, f := range in.LVs[i].Finalizers {
			if !wellKnown(f, in.Driver.Name) {
				a.note("finalizer", f)
			}
		}
	}
	for _, s := range in.NodeStates {
		for _, t := range s.Taints {
			if !wellKnown(t.Key, in.Driver.Name) {
				a.note("taint", t.Key)
			}
		}
	}
	for i := range in.StorageClasses {
		a.note("sc", in.StorageClasses[i].Name)
	}
	for i := range in.Capacities {
		a.note("sc", in.Capacities[i].StorageClassName)
	}
	for i := range in.PVs {
		pv := &in.PVs[i]
		a.note("pv", pv.Name)
		a.note("sc", pv.Spec.StorageClassName)
		if r := pv.Spec.ClaimRef; r != nil {
			a.note("ns", r.Namespace)
			a.note("claim", r.Name)
		}
		a.note("node", affinityNodes(pv)...)
	}
	for i := range in.PVCs {
		p := &in.PVCs[i]
		a.note("ns", p.Namespace)
		a.note("claim", p.Name)
		a.note("pv", p.Spec.VolumeName)
		a.note("node", p.Annotations[report.SelectedNodeAnnotation])
		if p.Spec.StorageClassName != nil {
			a.note("sc", *p.Spec.StorageClassName)
		}
	}
	for i := range in.Pods {
		p := &in.Pods[i]
		a.note("ns", p.Namespace)
		a.note("pod", p.Name)
		a.note("node", p.Spec.NodeName)
		for _, ref := range p.OwnerReferences {
			a.note("owner", ref.Name)
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				a.note("claim", v.PersistentVolumeClaim.ClaimName)
			}
		}
	}
	for i := range in.Events {
		e := &in.Events[i]
		a.note("event", e.Name)
		a.note("ns", e.Namespace, e.InvolvedObject.Namespace)
		switch e.InvolvedObject.Kind {
		case "Pod":
			a.note("pod", e.InvolvedObject.Name)
		case "PersistentVolumeClaim":
			a.note("claim", e.InvolvedObject.Name)
		}
	}
	for i := range in.CSINodes {
		a.note("node", in.CSINodes[i].Name)
	}
	for i := range in.TopoLVMPods {
		a.note("node", in.TopoLVMPods[i].Spec.NodeName)
	}
	key := in.Driver.TopologyKey()
	for i := range in.Capacities {
		if t := in.Capacities[i].NodeTopology; t != nil {
			a.note("node", t.MatchLabels[key])
		}
	}
}

func affinityNodes(pv *corev1.PersistentVolume) []string {
	var out []string
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return out
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, e := range term.MatchExpressions {
			out = append(out, e.Values...)
		}
	}
	return out
}

// Write dumps in into dir, which must not exist or be empty.
func Write(dir string, in report.Input, anonymize bool) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("directory %s is not empty", dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	in = keepTopoLVM(in)
	a := newAnonymizer(anonymize)
	a.collect(&in)
	a.freeze()
	w := &writer{dir: dir, a: a, in: &in}
	return w.all()
}

type writer struct {
	dir string
	a   *anonymizer
	in  *report.Input
}

func (w *writer) put(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.dir, name), append(data, '\n'), 0o600)
}

func list(kind string, items any) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": kind, "items": items}
}

func (w *writer) all() error {
	in, a := w.in, w.a
	absent := make([]string, 0, len(in.AbsentNodes))
	for n := range in.AbsentNodes {
		absent = append(absent, a.name("node", n))
	}
	sort.Strings(absent)
	meta := fixture.Meta{Driver: in.Driver.Name, Now: in.Now.UTC().Truncate(time.Minute), AbsentNodes: absent, Thin: in.Thin,
		SchedulerExtender: in.SchedulerExtender}
	steps := []func() error{
		func() error { return w.put("meta.json", meta) },
		func() error { return w.put("nodes.json", list("NodeList", w.nodes())) },
		func() error { return w.put("logicalvolumes.json", list("List", w.lvs())) },
	}
	if in.PVs != nil {
		steps = append(steps, func() error { return w.put("pvs.json", list("PersistentVolumeList", w.pvs())) })
	}
	if in.Classes != nil {
		steps = append(steps, func() error {
			return w.put("configmaps.json", list("ConfigMapList", w.configMaps()))
		})
	}
	if in.PVCs != nil {
		steps = append(steps, func() error { return w.put("pvcs.json", list("PersistentVolumeClaimList", w.pvcs())) })
	}
	if in.Pods != nil {
		steps = append(steps, func() error { return w.put("pods.json", list("PodList", w.pods(in.Pods, true))) })
	}
	if in.Events != nil {
		steps = append(steps, func() error { return w.put("events.json", list("EventList", w.events())) })
	}
	if in.StorageClasses != nil {
		steps = append(steps, func() error {
			return w.put("storageclasses.json", list("StorageClassList", w.storageClasses()))
		})
	}
	if in.Capacities != nil {
		steps = append(steps, func() error {
			return w.put("csistoragecapacities.json", list("CSIStorageCapacityList", w.capacities()))
		})
	}
	if in.CSINodes != nil {
		steps = append(steps, func() error { return w.put("csinodes.json", list("CSINodeList", w.csiNodes())) })
	}
	if in.CSIDriver != nil {
		steps = append(steps, func() error { return w.put("csidriver.json", csiDriver(in.CSIDriver)) })
	}
	if in.NodeStates != nil {
		steps = append(steps, func() error { return w.put("nodestates.json", w.nodeStates()) })
	}
	if in.ConfigPods != nil {
		steps = append(steps, func() error { return w.put("configpods.json", list("PodList", w.pods(in.ConfigPods, false))) })
	}
	if in.TopoLVMPods != nil {
		steps = append(steps, func() error { return w.put("topolvmpods.json", list("PodList", w.pods(in.TopoLVMPods, false))) })
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (w *writer) nodes() []map[string]any {
	d := w.in.Driver
	out := make([]map[string]any, 0, len(w.in.Nodes))
	for _, n := range w.in.Nodes {
		name := w.a.name("node", n.Name)
		ann := map[string]string{}
		for c, v := range n.Capacity {
			ann[d.CapacityPrefix()+c] = strconv.FormatInt(v, 10)
		}
		for _, bad := range n.BadAnnotations {
			ann[bad] = "unparsable"
		}
		out = append(out, map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{
			"name": name, "labels": map[string]string{d.TopologyKey(): name}, "annotations": ann,
		}})
	}
	return out
}

func (w *writer) lvs() []map[string]any {
	d, a := w.in.Driver, w.a
	out := make([]map[string]any, 0, len(w.in.LVs))
	for i := range w.in.LVs {
		lv := &w.in.LVs[i]
		md := map[string]any{"name": a.name("lv", lv.Name), "uid": lv.UID, "creationTimestamp": lv.Created.UTC().Format(time.RFC3339)}
		if len(lv.Finalizers) > 0 {
			fins := make([]string, 0, len(lv.Finalizers))
			for _, f := range lv.Finalizers {
				fins = append(fins, a.name("finalizer", f))
			}
			md["finalizers"] = fins
		}
		if lv.DeletionTimestamp != nil {
			md["deletionTimestamp"] = lv.DeletionTimestamp.UTC().Format(time.RFC3339)
		}
		ann := map[string]string{}
		for k, v := range lv.Annotations {
			if strings.HasPrefix(k, d.Name+"/") {
				ann[k] = v
			}
		}
		if len(ann) > 0 {
			md["annotations"] = ann
		}
		spec := map[string]any{"name": a.name("lv", lv.Name), "nodeName": a.name("node", lv.NodeName), "size": strconv.FormatInt(lv.SizeBytes, 10)}
		for k, v := range map[string]string{"deviceClass": lv.DeviceClass, "source": a.name("lv", lv.Source), "accessType": lv.AccessType} {
			if v != "" {
				spec[k] = v
			}
		}
		status := map[string]any{}
		if lv.CurrentSizeBytes != nil {
			status["currentSize"] = strconv.FormatInt(*lv.CurrentSizeBytes, 10)
		}
		if lv.VolumeID != "" {
			status["volumeID"] = lv.VolumeID
		}
		if lv.Code != 0 {
			status["code"] = lv.Code
		}
		if lv.Message != "" {
			status["message"] = a.message(lv.Message)
		}
		out = append(out, map[string]any{"apiVersion": d.Name + "/v1", "kind": "LogicalVolume", "metadata": md, "spec": spec, "status": status})
	}
	return out
}

func (w *writer) pvs() []corev1.PersistentVolume {
	a := w.a
	out := make([]corev1.PersistentVolume, 0, len(w.in.PVs))
	for i := range w.in.PVs {
		src := &w.in.PVs[i]
		pv := corev1.PersistentVolume{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolume"},
			ObjectMeta: metav1.ObjectMeta{Name: a.name("pv", src.Name), CreationTimestamp: src.CreationTimestamp},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:                      src.Spec.Capacity,
				StorageClassName:              a.name("sc", src.Spec.StorageClassName),
				PersistentVolumeReclaimPolicy: src.Spec.PersistentVolumeReclaimPolicy,
				VolumeMode:                    src.Spec.VolumeMode,
			},
			Status: corev1.PersistentVolumeStatus{Phase: src.Status.Phase, LastPhaseTransitionTime: src.Status.LastPhaseTransitionTime},
		}
		if c := src.Spec.CSI; c != nil {
			pv.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: c.Driver, VolumeHandle: c.VolumeHandle}
		}
		if r := src.Spec.ClaimRef; r != nil {
			pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: a.name("ns", r.Namespace), Name: a.name("claim", r.Name)}
		}
		if src.Spec.NodeAffinity != nil && src.Spec.NodeAffinity.Required != nil {
			req := src.Spec.NodeAffinity.Required.DeepCopy()
			for t := range req.NodeSelectorTerms {
				for e := range req.NodeSelectorTerms[t].MatchExpressions {
					vals := req.NodeSelectorTerms[t].MatchExpressions[e].Values
					for v := range vals {
						vals[v] = a.name("node", vals[v])
					}
				}
			}
			pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: req}
		}
		out = append(out, pv)
	}
	return out
}

func keepAnnotations(ann map[string]string, keep func(string) bool) map[string]string {
	out := map[string]string{}
	for k, v := range ann {
		if keep(k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func resizeAnnotation(k string) bool { return strings.HasPrefix(k, "resize.topolvm.io/") }

func (w *writer) pvcs() []corev1.PersistentVolumeClaim {
	a := w.a
	out := make([]corev1.PersistentVolumeClaim, 0, len(w.in.PVCs))
	for i := range w.in.PVCs {
		src := &w.in.PVCs[i]
		ann := keepAnnotations(src.Annotations, func(k string) bool { return k == report.SelectedNodeAnnotation || resizeAnnotation(k) })
		if node, ok := ann[report.SelectedNodeAnnotation]; ok {
			ann[report.SelectedNodeAnnotation] = a.name("node", node)
		}
		pvc := corev1.PersistentVolumeClaim{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
			ObjectMeta: metav1.ObjectMeta{Name: a.name("claim", src.Name), Namespace: a.name("ns", src.Namespace),
				CreationTimestamp: src.CreationTimestamp, Annotations: ann},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: a.scPtr(src.Spec.StorageClassName),
				VolumeName:       a.name("pv", src.Spec.VolumeName),
				Resources:        corev1.VolumeResourceRequirements{Requests: storageOnly(src.Spec.Resources.Requests)},
			},
			Status: corev1.PersistentVolumeClaimStatus{Phase: src.Status.Phase, Capacity: storageOnly(src.Status.Capacity)},
		}
		for _, c := range src.Status.Conditions {
			pvc.Status.Conditions = append(pvc.Status.Conditions, corev1.PersistentVolumeClaimCondition{
				Type: c.Type, Status: c.Status, LastTransitionTime: c.LastTransitionTime,
			})
		}
		out = append(out, pvc)
	}
	return out
}

func storageOnly(l corev1.ResourceList) corev1.ResourceList {
	if q, ok := l[corev1.ResourceStorage]; ok {
		return corev1.ResourceList{corev1.ResourceStorage: q}
	}
	return nil
}

// pods keeps identity, owner, node, claim volumes and readiness. renamed is false for TopoLVM's
// own pods, whose names carry nothing about the workloads.
func (w *writer) pods(src []corev1.Pod, renamed bool) []corev1.Pod {
	a := w.a
	out := make([]corev1.Pod, 0, len(src))
	for i := range src {
		p := &src[i]
		name, ns := p.Name, p.Namespace
		if renamed {
			name, ns = a.name("pod", p.Name), a.name("ns", p.Namespace)
		}
		pod := corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: p.CreationTimestamp},
			Spec:       corev1.PodSpec{NodeName: a.name("node", p.Spec.NodeName)},
			Status:     corev1.PodStatus{Phase: p.Status.Phase},
		}
		for _, ref := range p.OwnerReferences {
			pod.OwnerReferences = append(pod.OwnerReferences, metav1.OwnerReference{
				APIVersion: ref.APIVersion, Kind: ref.Kind, Name: a.name("owner", ref.Name), Controller: ref.Controller,
			})
		}
		if !renamed {
			for _, c := range p.Spec.Containers {
				pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: c.Name, Ports: c.Ports})
			}
		}
		for _, v := range p.Spec.Volumes {
			if !renamed && v.ConfigMap != nil {
				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: v.Name, VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: v.ConfigMap.LocalObjectReference},
				}})
			}
			if v.PersistentVolumeClaim != nil {
				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: fmt.Sprintf("v%d", len(pod.Spec.Volumes)), VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: a.name("claim", v.PersistentVolumeClaim.ClaimName)},
				}})
			}
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady {
				pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: c.Type, Status: c.Status})
			}
		}
		out = append(out, pod)
	}
	return out
}

func (w *writer) events() []corev1.Event {
	a := w.a
	out := make([]corev1.Event, 0, len(w.in.Events))
	for i := range w.in.Events {
		e := &w.in.Events[i]
		obj := e.InvolvedObject.Name
		switch e.InvolvedObject.Kind {
		case "Pod":
			obj = a.name("pod", obj)
		case "PersistentVolumeClaim":
			obj = a.name("claim", obj)
		case "PersistentVolume":
			obj = a.name("pv", obj)
		}
		out = append(out, corev1.Event{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
			ObjectMeta: metav1.ObjectMeta{Name: a.name("event", e.Name), Namespace: a.name("ns", e.Namespace)},
			InvolvedObject: corev1.ObjectReference{
				Kind: e.InvolvedObject.Kind, Namespace: a.name("ns", e.InvolvedObject.Namespace), Name: obj,
			},
			Reason: e.Reason, Message: a.message(e.Message), Type: e.Type, Count: e.Count,
			LastTimestamp: e.LastTimestamp, EventTime: e.EventTime,
		})
	}
	return out
}

func (w *writer) storageClasses() []storagev1.StorageClass {
	src := w.in.StorageClasses
	out := make([]storagev1.StorageClass, 0, len(src))
	for i := range src {
		s := &src[i]
		out = append(out, storagev1.StorageClass{
			TypeMeta:             metav1.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass"},
			ObjectMeta:           metav1.ObjectMeta{Name: w.a.name("sc", s.Name), Annotations: keepAnnotations(s.Annotations, resizeAnnotation)},
			Provisioner:          s.Provisioner,
			Parameters:           s.Parameters,
			VolumeBindingMode:    s.VolumeBindingMode,
			AllowVolumeExpansion: s.AllowVolumeExpansion,
			ReclaimPolicy:        s.ReclaimPolicy,
		})
	}
	return out
}

func (w *writer) capacities() []storagev1.CSIStorageCapacity {
	key := w.in.Driver.TopologyKey()
	out := make([]storagev1.CSIStorageCapacity, 0, len(w.in.Capacities))
	for i := range w.in.Capacities {
		c := &w.in.Capacities[i]
		cc := storagev1.CSIStorageCapacity{
			TypeMeta:         metav1.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "CSIStorageCapacity"},
			ObjectMeta:       metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace},
			StorageClassName: w.a.name("sc", c.StorageClassName),
			Capacity:         c.Capacity,
		}
		if c.NodeTopology != nil {
			cc.NodeTopology = &metav1.LabelSelector{MatchLabels: map[string]string{key: w.a.name("node", c.NodeTopology.MatchLabels[key])}}
		}
		out = append(out, cc)
	}
	return out
}

func (w *writer) csiNodes() []storagev1.CSINode {
	out := make([]storagev1.CSINode, 0, len(w.in.CSINodes))
	for i := range w.in.CSINodes {
		n := &w.in.CSINodes[i]
		cn := storagev1.CSINode{
			TypeMeta:   metav1.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "CSINode"},
			ObjectMeta: metav1.ObjectMeta{Name: w.a.name("node", n.Name)},
		}
		for _, d := range n.Spec.Drivers {
			if d.Name == w.in.Driver.Name {
				cn.Spec.Drivers = append(cn.Spec.Drivers, storagev1.CSINodeDriver{Name: d.Name, NodeID: w.a.name("node", d.NodeID), TopologyKeys: d.TopologyKeys})
			}
		}
		out = append(out, cn)
	}
	return out
}

func csiDriver(d *storagev1.CSIDriver) storagev1.CSIDriver {
	return storagev1.CSIDriver{
		TypeMeta:   metav1.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "CSIDriver"},
		ObjectMeta: metav1.ObjectMeta{Name: d.Name},
		Spec:       storagev1.CSIDriverSpec{StorageCapacity: d.Spec.StorageCapacity, AttachRequired: d.Spec.AttachRequired},
	}
}

func (w *writer) nodeStates() map[string]report.NodeState {
	out := map[string]report.NodeState{}
	for n, s := range w.in.NodeStates {
		if w.a.on {
			taints := make([]corev1.Taint, 0, len(s.Taints))
			for _, t := range s.Taints {
				taints = append(taints, corev1.Taint{Key: w.a.name("taint", t.Key), Effect: t.Effect})
			}
			s.Taints = taints
		}
		out[w.a.name("node", n)] = s
	}
	return out
}

// configMaps writes the lvmd ConfigMaps as read, keeping only their name, label and lvmd.yaml; on
// LVMS, where classes come from LVMVolumeGroups, it writes one ConfigMap with the same classes.
func (w *writer) configMaps() []corev1.ConfigMap {
	if len(w.in.LVMDConfigMaps) == 0 {
		return []corev1.ConfigMap{lvmdConfigMap(w.in.Classes)}
	}
	out := make([]corev1.ConfigMap, 0, len(w.in.LVMDConfigMaps))
	for i := range w.in.LVMDConfigMaps {
		cm := &w.in.LVMDConfigMaps[i]
		out = append(out, corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: cm.Name, Labels: map[string]string{"app.kubernetes.io/name": "topolvm"}},
			Data:       map[string]string{"lvmd.yaml": cm.Data["lvmd.yaml"]},
		})
	}
	return out
}

// lvmdConfigMap writes the device classes back as an lvmd.yaml that parses to the same classes.
func lvmdConfigMap(classes map[string]topolvm.DeviceClass) corev1.ConfigMap {
	names := make([]string, 0, len(classes))
	for n := range classes {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("device-classes:\n")
	for _, n := range names {
		dc := classes[n]
		fmt.Fprintf(&b, "  - name: %s\n    volume-group: %s\n", dc.Name, dc.VolumeGroup)
		if dc.Default {
			b.WriteString("    default: true\n")
		}
		switch dc.Type {
		case topolvm.TypeThin:
			fmt.Fprintf(&b, "    type: thin\n    thin-pool:\n      name: %s\n      overprovision-ratio: %g\n", dc.ThinPool, dc.OverprovisionRatio)
		case topolvm.TypeUnknown:
			b.WriteString("    type: unknown\n")
		}
		if dc.SpareBytes != nil {
			fmt.Fprintf(&b, "    spare-gb: %d\n", *dc.SpareBytes>>30)
		}
	}
	return corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "topolvm-lvmd-0", Labels: map[string]string{"app.kubernetes.io/name": "topolvm"}},
		Data:       map[string]string{"lvmd.yaml": b.String()},
	}
}

func (a *anonymizer) scPtr(p *string) *string {
	if p == nil {
		return nil
	}
	return new(a.name("sc", *p))
}

// keepTopoLVM drops everything that is not about TopoLVM volumes: other drivers' PVs and
// StorageClasses, their claims, and pods and events not tied to a TopoLVM claim. A nil field stays
// nil, so "not collected" survives the dump.
func keepTopoLVM(in report.Input) report.Input {
	d := in.Driver.Name
	ours := map[string]bool{}
	if in.StorageClasses != nil {
		scs := []storagev1.StorageClass{}
		for i := range in.StorageClasses {
			if in.StorageClasses[i].Provisioner == d {
				ours[in.StorageClasses[i].Name] = true
				scs = append(scs, in.StorageClasses[i])
			}
		}
		in.StorageClasses = scs
	}
	pvNames := map[string]bool{}
	if in.PVs != nil {
		pvs := []corev1.PersistentVolume{}
		for i := range in.PVs {
			if c := in.PVs[i].Spec.CSI; c != nil && c.Driver == d {
				pvNames[in.PVs[i].Name] = true
				pvs = append(pvs, in.PVs[i])
			}
		}
		in.PVs = pvs
	}
	claims := map[string]bool{}
	if in.PVCs != nil {
		pvcs := []corev1.PersistentVolumeClaim{}
		for i := range in.PVCs {
			p := &in.PVCs[i]
			if (p.Spec.StorageClassName != nil && ours[*p.Spec.StorageClassName]) || pvNames[p.Spec.VolumeName] {
				claims[p.Namespace+"/"+p.Name] = true
				pvcs = append(pvcs, *p)
			}
		}
		in.PVCs = pvcs
	}
	pods := map[string]bool{}
	if in.Pods != nil {
		kept := []corev1.Pod{}
		for i := range in.Pods {
			p := &in.Pods[i]
			for _, v := range p.Spec.Volumes {
				if v.PersistentVolumeClaim != nil && claims[p.Namespace+"/"+v.PersistentVolumeClaim.ClaimName] {
					pods[p.Namespace+"/"+p.Name] = true
					kept = append(kept, *p)
					break
				}
			}
		}
		in.Pods = kept
	}
	if in.Capacities != nil && in.StorageClasses != nil {
		kept := []storagev1.CSIStorageCapacity{}
		for i := range in.Capacities {
			if ours[in.Capacities[i].StorageClassName] {
				kept = append(kept, in.Capacities[i])
			}
		}
		in.Capacities = kept
	}
	if in.CSINodes != nil {
		kept := []storagev1.CSINode{}
		for i := range in.CSINodes {
			for _, drv := range in.CSINodes[i].Spec.Drivers {
				if drv.Name == d {
					kept = append(kept, in.CSINodes[i])
					break
				}
			}
		}
		in.CSINodes = kept
	}
	if in.Events != nil {
		kept := []corev1.Event{}
		for i := range in.Events {
			e := &in.Events[i]
			key := e.InvolvedObject.Namespace + "/" + e.InvolvedObject.Name
			if (e.InvolvedObject.Kind == "Pod" && pods[key]) || (e.InvolvedObject.Kind == "PersistentVolumeClaim" && claims[key]) ||
				(e.InvolvedObject.Kind == "PersistentVolume" && pvNames[e.InvolvedObject.Name]) {
				kept = append(kept, *e)
			}
		}
		in.Events = kept
	}
	return in
}
