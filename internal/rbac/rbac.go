// Package rbac prints the roles the plugin needs and checks them against the current user.
package rbac

import (
	"context"
	"fmt"
	"slices"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Features are the optional modes that need more rights than the base role.
var Features = []string{"metrics", "usage", "lvms"}

// Rule is one RBAC rule, cluster-wide unless Namespaced.
type Rule struct {
	Group      string
	Resources  []string
	Verbs      []string
	Namespaced bool
	Comment    string
}

const roleName = "kubectl-topolvm-view"

const usageWarning = "# nodes/proxy reaches the kubelet API, which includes exec into any pod on the node.\n" +
	"  # Grant it only to people who may already exec on nodes."

// Rules lists what the plugin reads, plus what the requested features add.
func Rules(with []string) ([]Rule, error) {
	for _, f := range with {
		if !slices.Contains(Features, f) {
			return nil, fmt.Errorf("unknown feature %q; want %s", f, strings.Join(Features, ", "))
		}
	}
	rules := []Rule{
		{Group: "", Resources: []string{"nodes"}, Verbs: []string{"get", "list"}},
		{Group: "", Resources: []string{"persistentvolumes", "persistentvolumeclaims", "pods", "events"}, Verbs: []string{"list"}},
		{Group: "storage.k8s.io", Resources: []string{"csidrivers"}, Verbs: []string{"get"}},
		{Group: "storage.k8s.io", Resources: []string{"storageclasses", "csinodes", "csistoragecapacities"}, Verbs: []string{"list"}},
		{Group: "topolvm.io", Resources: []string{"logicalvolumes"}, Verbs: []string{"list"}},
		{Group: "topolvm.cybozu.com", Resources: []string{"logicalvolumes"}, Verbs: []string{"list"}},
	}
	if slices.Contains(with, "lvms") {
		rules = append(rules, Rule{Group: "lvm.topolvm.io", Resources: []string{"lvmvolumegroups"}, Verbs: []string{"list"}})
	}
	if slices.Contains(with, "usage") {
		rules = append(rules, Rule{Group: "", Resources: []string{"nodes/proxy"}, Verbs: []string{"get"}, Comment: usageWarning})
	}
	rules = append(rules, Rule{Group: "", Resources: []string{"configmaps", "pods"}, Verbs: []string{"list"}, Namespaced: true})
	if slices.Contains(with, "metrics") {
		rules = append(rules, Rule{Group: "", Resources: []string{"pods/proxy"}, Verbs: []string{"get"}, Namespaced: true})
	}
	return rules, nil
}

func writeRules(b *strings.Builder, rules []Rule, namespaced bool) {
	b.WriteString("rules:\n")
	for _, r := range rules {
		if r.Namespaced != namespaced {
			continue
		}
		if r.Comment != "" {
			fmt.Fprintf(b, "  %s\n", r.Comment)
		}
		group := r.Group
		if group == "" {
			group = `""`
		}
		fmt.Fprintf(b, "  - apiGroups: [%s]\n    resources: [%s]\n    verbs: [%s]\n", group, strings.Join(r.Resources, ", "), strings.Join(r.Verbs, ", "))
	}
}

// YAML renders a ClusterRole and a Role in the TopoLVM namespace, ready for kubectl apply.
func YAML(with []string, namespace string) (string, error) {
	rules, err := Rules(with)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: %s\n", roleName)
	writeRules(&b, rules, false)
	fmt.Fprintf(&b, "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: %s\n  namespace: %s\n", roleName, namespace)
	writeRules(&b, rules, true)
	return b.String(), nil
}

// Result is one permission and whether the current user has it.
type Result struct {
	Verb      string `json:"verb"`
	Group     string `json:"group"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace"`
	Allowed   bool   `json:"allowed"`
	Reason    string `json:"reason,omitempty"`
}

// Check asks the API server, through SelfSubjectAccessReview, whether the current user holds every
// permission the plugin needs. A review is created and answered in place; nothing is stored.
func Check(ctx context.Context, core kubernetes.Interface, with []string, namespace string) ([]Result, error) {
	rules, err := Rules(with)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, r := range rules {
		ns := ""
		if r.Namespaced {
			ns = namespace
		}
		for _, res := range r.Resources {
			resource, sub, _ := strings.Cut(res, "/")
			for _, verb := range r.Verbs {
				review := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authv1.ResourceAttributes{Namespace: ns, Verb: verb, Group: r.Group, Resource: resource, Subresource: sub},
				}}
				got, err := core.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
				if err != nil {
					return nil, fmt.Errorf("selfsubjectaccessreview for %s %s: %w", verb, res, err)
				}
				out = append(out, Result{Verb: verb, Group: r.Group, Resource: res, Namespace: ns, Allowed: got.Status.Allowed, Reason: got.Status.Reason})
			}
		}
	}
	return out, nil
}
