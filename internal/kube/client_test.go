package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

func TestRawKeepsServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/metrics") {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte("topolvm_volumegroup_size_bytes 1\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"nodes \"a\" is forbidden: ` +
			`User \"viewer\" cannot get resource \"nodes/proxy\"","reason":"Forbidden","code":403}`))
	}))
	defer srv.Close()
	c, err := NewClients(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Raw(context.Background(), "/api/v1/nodes/a/proxy/stats/summary")
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), `cannot get resource "nodes/proxy"`) {
		t.Errorf("err = %v", err)
	}
	body, err := c.Raw(context.Background(), "/api/v1/namespaces/x/pods/p:8080/proxy/metrics")
	if err != nil || string(body) != "topolvm_volumegroup_size_bytes 1\n" {
		t.Errorf("body = %q, err = %v", body, err)
	}
}
