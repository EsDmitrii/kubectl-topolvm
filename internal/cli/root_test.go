package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCommandName(t *testing.T) {
	for in, want := range map[string]string{
		"kubectl-topolvm":                "topolvm",
		"/usr/local/bin/kubectl-topolvm": "topolvm",
		"kubectl-topolvm.exe":            "topolvm",
		"kubectl-topolvm_capacity":       "topolvm-capacity",
		"kubectl-topolvm_capacity.exe":   "topolvm-capacity",
		"topolvm-dev":                    "topolvm-dev",
	} {
		if got := commandName(in); got != want {
			t.Errorf("commandName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{fmt.Errorf("x: %w", errFindings), exitFindings},
		{errors.New("boom"), exitError},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("exitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionJSON(t *testing.T) {
	code, out, errOut := runCLI(t, "version", "-o", "json")
	if code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	want := map[string]string{"version": "dev", "commit": "none", "date": "unknown"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestKubectlFlags(t *testing.T) {
	root := newRootCmd(newGlobalOptions())
	pf := root.PersistentFlags()
	for _, name := range []string{"context", "kubeconfig", "namespace", "as", "request-timeout",
		"output", "no-headers", "node", "device-class", "driver", "topolvm-namespace", "thin"} {
		if pf.Lookup(name) == nil {
			t.Errorf("flag --%s is not registered", name)
		}
	}
	if f := pf.ShorthandLookup("n"); f == nil || f.Name != "namespace" {
		t.Errorf("-n is not --namespace")
	}
	if f := pf.ShorthandLookup("o"); f == nil || f.Name != "output" {
		t.Errorf("-o is not --output")
	}
	if f := pf.Lookup("topolvm-namespace"); f != nil && f.DefValue != "topolvm-system" {
		t.Errorf("--topolvm-namespace default = %q", f.DefValue)
	}
}

func TestInvalidOutput(t *testing.T) {
	code, _, errOut := runCLI(t, "version", "-o", "csv")
	if code != exitError {
		t.Fatalf("exit %d, want %d", code, exitError)
	}
	want := `invalid output format "csv": want table, wide, json or yaml`
	if !strings.Contains(errOut, want) {
		t.Errorf("stderr %q does not contain %q", errOut, want)
	}
}
