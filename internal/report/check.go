package report

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
	"github.com/EsDmitrii/kubectl-topolvm/internal/units"
)

// CheckIDs lists every check in the order findings are reported.
var CheckIDs = []string{
	"orphan-lv", "pv-without-lv", "released-pv", "lv-on-missing-node",
	"stuck-deletion", "pending-deletion", "lv-error", "resize-pending", "uid-mismatch",
}

// Finding is one problem a check found.
type Finding struct {
	Check       string `json:"check"`
	Object      string `json:"object"`
	Node        string `json:"node"`
	DeviceClass string `json:"deviceClass"`
	Namespace   string `json:"namespace"`
	SizeBytes   *int64 `json:"sizeBytes"`
	AgeSeconds  *int64 `json:"ageSeconds"`
	Detail      string `json:"detail"`
}

var grpcCodes = []string{
	"OK", "Canceled", "Unknown", "InvalidArgument", "DeadlineExceeded", "NotFound", "AlreadyExists",
	"PermissionDenied", "ResourceExhausted", "FailedPrecondition", "Aborted", "OutOfRange",
	"Unimplemented", "Internal", "Unavailable", "DataLoss", "Unauthenticated",
}

// codeName names the gRPC code lvmd stored in status.code.
func codeName(code int64) string {
	if code >= 0 && code < int64(len(grpcCodes)) {
		return grpcCodes[code]
	}
	return fmt.Sprintf("code %d", code)
}

// parseGoTime reads a time written with Go's time.String(), which TopoLVM uses for
// resize-requested-at: "2006-01-02 15:04:05.999999999 -0700 MST" plus an optional monotonic part.
func parseGoTime(s string) (time.Time, error) {
	s, _, _ = strings.Cut(s, " m=")
	return time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", s)
}

type checker struct {
	in       *Input
	minAge   time.Duration
	findings []Finding
}

func (c *checker) age(t time.Time) time.Duration { return max(c.in.Now.Sub(t), 0) }

// scope carries the device class and namespace a finding is about, for --device-class and -n.
type scope struct {
	c         *checker
	class, ns string
}

func (c *checker) about(class, namespace string) scope { return scope{c, class, namespace} }

// add records a cluster-level finding: no device class, namespace, size or age.
func (c *checker) add(check, object, node, detail string) {
	c.about("", "").add(check, object, node, nil, nil, detail)
}

func (s scope) add(check, object, node string, size *int64, age *time.Duration, detail string) {
	f := Finding{Check: check, Object: object, Node: node, DeviceClass: s.class, Namespace: s.ns, SizeBytes: size, Detail: detail}
	if age != nil {
		f.AgeSeconds = new(int64(age.Seconds()))
	}
	s.c.findings = append(s.c.findings, f)
}

func claimNamespace(pv *corev1.PersistentVolume) string {
	if pv == nil || pv.Spec.ClaimRef == nil {
		return ""
	}
	return pv.Spec.ClaimRef.Namespace
}

// recentPhase reports a PV phase change younger than minAge, while the provisioner may still be
// reclaiming the volume. Clusters older than 1.31 do not record the time and are never recent.
func (c *checker) recentPhase(pv *corev1.PersistentVolume) bool {
	t := pv.Status.LastPhaseTransitionTime
	return t != nil && c.age(t.Time) < c.minAge
}

const removedFromDisk = "the LV is already removed from disk"

// releasedDetail explains a Released PV. Under Delete it stays only when the provisioner failed to
// delete the volume, and its last VolumeFailedDelete event says why.
func (c *checker) releasedDetail(pv *corev1.PersistentVolume, onDisk bool) string {
	policy := pv.Spec.PersistentVolumeReclaimPolicy
	detail := fmt.Sprintf("PV is Released (reclaimPolicy %s)", policy)
	if policy == corev1.PersistentVolumeReclaimDelete {
		detail += " and was not deleted"
	}
	if onDisk {
		detail += ", the LV still holds the space"
	} else {
		detail += ", " + removedFromDisk
	}
	if policy == corev1.PersistentVolumeReclaimDelete {
		if e := lastEvent(c.in, "PersistentVolume", "", pv.Name, "VolumeFailedDelete"); e != "" {
			detail += "; last error: " + e
		}
	}
	return detail
}

func pvNode(pv *corev1.PersistentVolume, key string) string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return ""
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, e := range term.MatchExpressions {
			if e.Key == key && len(e.Values) > 0 {
				return e.Values[0]
			}
		}
	}
	return ""
}

func buildFindings(in Input, o Options) (findings []Finding, warnings []string) {
	c := &checker{in: &in, minAge: o.MinAge}
	pvs := newPVIndex(&in)
	classOf := classResolver(&in)
	d := in.Driver

	if in.PVs != nil {
		keys, byName := map[string]bool{}, map[string]*topolvm.LogicalVolume{}
		for i := range in.LVs {
			keys[in.LVs[i].Key()], byName[in.LVs[i].Name] = true, &in.LVs[i]
		}
		for _, k := range in.SkippedLVs {
			keys[k] = true
		}
		for i := range in.PVs {
			pv := &in.PVs[i]
			if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != d.Name || keys[pv.Spec.CSI.VolumeHandle] {
				continue
			}
			if a := c.age(pv.CreationTimestamp.Time); a >= c.minAge && !c.recentPhase(pv) {
				detail := "no LogicalVolume with volumeID " + pv.Spec.CSI.VolumeHandle
				if lv := byName[pv.Name]; lv != nil {
					detail += fmt.Sprintf("; LogicalVolume %s has volumeID %s, and TopoLVM finds volumes only by volumeID", lv.Name, lv.Key())
				}
				c.about("", claimNamespace(pv)).add("pv-without-lv", "persistentvolume/"+pv.Name, pvNode(pv, d.TopologyKey()), nil, &a, detail)
			}
		}
	} else {
		warnings = append(warnings, "persistentvolumes not collected: orphan-lv, pv-without-lv and released-pv skipped")
	}

	for i := range in.LVs {
		lv := &in.LVs[i]
		object := "logicalvolume/" + lv.Name
		onDisk := lv.OnDisk(d.LVFinalizer())
		var size *int64
		if onDisk {
			size = new(lv.UsedBytes())
		}
		created := c.age(lv.Created)
		var pv *corev1.PersistentVolume
		if in.PVs != nil {
			pv = pvs.find(lv)
		}
		s := c.about(classOf(lv), claimNamespace(pv))

		if in.PVs != nil {
			switch {
			case pv == nil && !lv.IsSnapshot() && lv.VolumeID != "" && lv.DeletionTimestamp == nil && created >= c.minAge:
				s.add("orphan-lv", object, lv.NodeName, size, &created, "no PersistentVolume with volumeHandle "+lv.Key())
			case pv != nil && pv.Status.Phase == corev1.VolumeReleased && !c.recentPhase(pv):
				var age *time.Duration
				if t := pv.Status.LastPhaseTransitionTime; t != nil {
					age = new(c.age(t.Time))
				}
				s.add("released-pv", "persistentvolume/"+pv.Name, lv.NodeName, size, age, c.releasedDetail(pv, onDisk))
			}
		}
		if in.AbsentNodes[lv.NodeName] {
			s.add("lv-on-missing-node", object, lv.NodeName, size, &created, fmt.Sprintf("node %s no longer exists", lv.NodeName))
		}
		if lv.DeletionTimestamp != nil {
			if a := c.age(*lv.DeletionTimestamp); a >= c.minAge {
				fin := none
				if len(lv.Finalizers) > 0 {
					fin = strings.Join(lv.Finalizers, ", ")
				}
				detail := fmt.Sprintf("deletion requested %s ago, finalizers: %s", duration.HumanDuration(a), fin)
				if !onDisk {
					detail += "; " + removedFromDisk
				}
				s.add("stuck-deletion", object, lv.NodeName, size, &a, detail)
			}
		}
		// The node controller sets the annotation and drops the finalizer in one patch. Both at once
		// means someone wrote them, and topolvm-node then skips the LV for good.
		_, marked := lv.Annotations[d.PendingDeletionKey()]
		if marked && slices.Contains(lv.Finalizers, d.LVFinalizer()) {
			s.add("pending-deletion", object, lv.NodeName, size, &created,
				fmt.Sprintf("%s is set but finalizer %s is still there", d.PendingDeletionKey(), d.LVFinalizer()))
		}
		if lv.Code != 0 {
			what := "expand"
			if lv.VolumeID == "" {
				what = "create of " + units.FormatIEC(lv.SizeBytes)
			}
			s.add("lv-error", object, lv.NodeName, size, &created, fmt.Sprintf("%s failed: %s: %s", what, codeName(lv.Code), lv.Message))
		}
		if lv.CurrentSizeBytes != nil && *lv.CurrentSizeBytes < lv.SizeBytes {
			var age *time.Duration
			if t, err := parseGoTime(lv.Annotations[d.ResizeRequestedAtKey()]); err == nil {
				a := c.age(t)
				age = &a
			}
			if age == nil || *age >= c.minAge {
				s.add("resize-pending", object, lv.NodeName, new(lv.SizeBytes), age,
					fmt.Sprintf("spec.size %s, current %s", units.FormatIEC(lv.SizeBytes), units.FormatIEC(*lv.CurrentSizeBytes)))
			}
		}
		if lv.VolumeID != "" && lv.VolumeID != lv.UID {
			s.add("uid-mismatch", object, lv.NodeName, size, &created,
				fmt.Sprintf("status.volumeID %s differs from metadata.uid %s: the CR was recreated", lv.VolumeID, lv.UID))
		}
	}
	c.extended()
	return c.result(o), warnings
}

func (c *checker) result(o Options) []Finding {
	skip, nodes, classes := set(o.Skip), set(o.Nodes), set(o.DeviceClasses)
	order := map[string]int{}
	for i, id := range CheckIDs {
		order[id] = i
	}
	out := []Finding{}
	for _, f := range c.findings {
		if !skip[f.Check] && allowed(nodes, f.Node) && allowed(classes, f.DeviceClass) && (o.Namespace == "" || f.Namespace == o.Namespace) {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if order[a.Check] != order[b.Check] {
			return order[a.Check] < order[b.Check]
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		return a.Object < b.Object
	})
	return out
}

const none = "<none>"
