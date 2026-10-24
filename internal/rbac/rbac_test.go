package rbac

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	runtime2 "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite docs/rbac.yaml")

func docsPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "docs", "rbac.yaml")
}

func docsRBAC(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(docsPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRBACDefaultGolden(t *testing.T) {
	got, err := YAML(nil, "topolvm-system")
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(docsPath(), []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got != docsRBAC(t) {
		t.Errorf("docs/rbac.yaml is out of date; rbac prints:\n%s", got)
	}
	for _, want := range []string{"kind: ClusterRole", "kind: Role", "namespace: topolvm-system", "logicalvolumes", "csistoragecapacities"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(got, "nodes/proxy") || strings.Contains(got, "pods/proxy") || strings.Contains(got, "secrets") {
		t.Errorf("the base role must not grant proxies or secrets:\n%s", got)
	}
}

func TestRBACWithUsageWarns(t *testing.T) {
	got, err := YAML([]string{"usage", "metrics", "lvms"}, "openshift-storage")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nodes/proxy", "# nodes/proxy", "pods/proxy", "lvmvolumegroups", "namespace: openshift-storage"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if _, err := YAML([]string{"everything"}, "x"); err == nil || !strings.Contains(err.Error(), "metrics, usage, lvms") {
		t.Errorf("unknown feature: %v", err)
	}
}

func TestRBACCheck(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime2.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		r.Status.Allowed = r.Spec.ResourceAttributes.Resource != "csinodes"
		return true, r, nil
	})
	results, err := Check(context.Background(), cs, nil, "topolvm-system")
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	for _, r := range results {
		if !r.Allowed {
			denied++
			if r.Resource != "csinodes" {
				t.Errorf("unexpected denial: %+v", r)
			}
		}
	}
	if denied != 1 || len(results) < 10 {
		t.Errorf("denied = %d of %d", denied, len(results))
	}
}

func TestRBACParsesAsRoles(t *testing.T) {
	out, err := YAML([]string{"metrics", "usage", "lvms"}, "topolvm-system")
	if err != nil {
		t.Fatal(err)
	}
	docs := strings.Split(strings.TrimPrefix(out, "---\n"), "\n---\n")
	if len(docs) != 2 {
		t.Fatalf("want 2 documents, got %d", len(docs))
	}
	var cr rbacv1.ClusterRole
	var role rbacv1.Role
	if err := yaml.UnmarshalStrict([]byte(docs[0]), &cr); err != nil {
		t.Fatalf("cluster role: %v", err)
	}
	if err := yaml.UnmarshalStrict([]byte(docs[1]), &role); err != nil {
		t.Fatalf("role: %v", err)
	}
	if cr.Kind != "ClusterRole" || len(cr.Rules) != 8 || role.Namespace != "topolvm-system" || len(role.Rules) != 2 {
		t.Errorf("cluster role %d rules, role %q %d rules", len(cr.Rules), role.Namespace, len(role.Rules))
	}
	for _, r := range cr.Rules {
		for _, v := range r.Verbs {
			if v != "get" && v != "list" {
				t.Errorf("only get and list are allowed, got %q", v)
			}
		}
	}
}

func TestReadmeRBAC(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(filepath.Dir(docsPath()), "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "```yaml\n"+docsRBAC(t)+"```") {
		t.Error("README.md must contain docs/rbac.yaml verbatim in a yaml block")
	}
	if !strings.Contains(string(readme), "\n## RBAC\n") {
		t.Error(`README.md lost the "## RBAC" heading the RBAC hint links to`)
	}
}
