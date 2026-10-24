package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

// collectFn reads cluster state for a command. Tests swap it for a fixture loader. req carries what
// the command needs; the cluster-wide fields (driver, namespace, flags, time) are filled here.
var collectFn = func(ctx context.Context, o *globalOptions, req kube.CollectOptions) (report.Input, error) {
	cfg, err := kube.RESTConfig(o.rules, o.overrides)
	if err != nil {
		return report.Input{}, err
	}
	c, err := kube.NewClients(cfg)
	if err != nil {
		return report.Input{}, err
	}
	d, warnings, err := kube.DetectDriver(ctx, c.Core, o.driver)
	if err != nil {
		return report.Input{}, err
	}
	req.Driver, req.TopoLVMNamespace, req.Thin, req.Now = d, o.topolvmNamespace, o.thin, time.Now().UTC()
	in, err := kube.Collect(ctx, c, req)
	if err != nil {
		return report.Input{}, err
	}
	in.Warnings = append(warnings, in.Warnings...)
	return in, nil
}

// reportOptions maps the shared flags. The namespace filter applies only when -n is given: the
// namespace of the kubeconfig context must not silently hide volumes.
func (o *globalOptions) reportOptions(cmd *cobra.Command) report.Options {
	ro := report.Options{Nodes: o.nodes, DeviceClasses: o.deviceClasses, MinAge: defaultMinAge}
	if f := cmd.Flags().Lookup("namespace"); f != nil && f.Changed {
		ro.Namespace = o.overrides.Context.Namespace
	}
	return ro
}

const defaultMinAge = 15 * time.Minute

func (o *globalOptions) structured() bool { return o.output == "json" || o.output == "yaml" }

func (o *globalOptions) tableOptions(cmd *cobra.Command) output.TableOptions {
	return output.TableOptions{
		Wide:      o.output == "wide",
		NoHeaders: o.noHeaders,
		Bytes:     o.bytes,
		Color:     o.useColor(cmd.OutOrStdout()),
	}
}

// useColor follows --color; auto colors a terminal unless NO_COLOR is set or TERM is dumb.
func (o *globalOptions) useColor(w io.Writer) bool {
	switch o.color {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// collect validates the output flag and reads cluster state.
func (o *globalOptions) collect(cmd *cobra.Command, req kube.CollectOptions) (report.Input, error) {
	if err := o.validateOutput(); err != nil {
		return report.Input{}, err
	}
	if err := o.validateColor(); err != nil {
		return report.Input{}, err
	}
	in, err := collectFn(cmd.Context(), o, req)
	if err != nil {
		return report.Input{}, err
	}
	if err := in.UnknownFilter(o.nodes, o.deviceClasses); err != nil {
		return report.Input{}, err
	}
	return in, nil
}

// warn prints warnings to stderr for table output; structured output carries them in the document.
func (o *globalOptions) warn(cmd *cobra.Command, warnings []string) error {
	if o.structured() {
		return nil
	}
	for _, w := range warnings {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w); err != nil {
			return err
		}
	}
	return nil
}

// build collects and computes the report shared by most commands, printing its warnings.
func (o *globalOptions) build(cmd *cobra.Command, req kube.CollectOptions, tune func(*report.Options)) (report.Report, error) {
	in, err := o.collect(cmd, req)
	if err != nil {
		return report.Report{}, err
	}
	ro := o.reportOptions(cmd)
	if tune != nil {
		tune(&ro)
	}
	r := report.Build(in, ro)
	return r, o.warn(cmd, r.Warnings)
}

// emit writes a structured document or calls table for text output.
func (o *globalOptions) emit(cmd *cobra.Command, doc any, table func() error) error {
	if o.structured() {
		return output.Encode(cmd.OutOrStdout(), doc, o.output)
	}
	return table()
}

// note prints a line to stderr for table output only.
func (o *globalOptions) note(cmd *cobra.Command, format string, args ...any) error {
	if o.structured() {
		return nil
	}
	_, err := fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
	return err
}

// namespace is -n, else the namespace of the kubeconfig context, else default.
func (o *globalOptions) namespace(cmd *cobra.Command) string {
	if f := cmd.Flags().Lookup("namespace"); f != nil && f.Changed {
		return o.overrides.Context.Namespace
	}
	if ns, _, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(o.rules, o.overrides).Namespace(); err == nil && ns != "" {
		return ns
	}
	return "default"
}

func parseSize(s string) (int64, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil || q.Sign() <= 0 {
		return 0, fmt.Errorf("invalid size %q: want a positive quantity such as 50Gi", s)
	}
	return q.Value(), nil
}
