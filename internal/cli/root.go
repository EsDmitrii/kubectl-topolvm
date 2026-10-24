// Package cli implements the kubectl-topolvm command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	// Cloud-provider auth plugins (OIDC, exec and friends): without this import client-go cannot
	// authenticate against many managed clusters. See
	// https://krew.sigs.k8s.io/docs/developer-guide/develop/best-practices/
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

// exit codes returned by Execute.
const (
	exitOK       = 0
	exitError    = 1
	exitFindings = 2 // a check found problems or a threshold was breached
)

// errFindings signals that a command completed but found problems, so Execute can map it to exit
// code 2 for scripting.
var errFindings = errors.New("check found problems")

// globalOptions holds flags shared by every subcommand.
type globalOptions struct {
	output           string
	noHeaders        bool
	nodes            []string
	deviceClasses    []string
	thin             []string
	driver           string
	topolvmNamespace string
	bytes            bool
	color            string
	rules            *clientcmd.ClientConfigLoadingRules
	overrides        *clientcmd.ConfigOverrides
}

func newGlobalOptions() *globalOptions {
	return &globalOptions{
		rules:     clientcmd.NewDefaultClientConfigLoadingRules(),
		overrides: &clientcmd.ConfigOverrides{},
	}
}

// Execute builds and runs the root command, returning a process exit code. The first SIGINT or
// SIGTERM cancels the command context so an in-flight request does not hang; a second one kills
// the process.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := newRootCmd(newGlobalOptions())
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	code := exitCode(err)
	if code == exitError {
		_, _ = fmt.Fprintln(stderr, "error: "+err.Error())
	}
	return code
}

// exitCode maps a command's error to the process exit code.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errFindings):
		return exitFindings
	default:
		return exitError
	}
}

// commandName turns the binary name into the command users type: kubectl-topolvm becomes topolvm,
// and krew's symlink kubectl-topolvm_capacity becomes topolvm-capacity.
func commandName(arg0 string) string {
	base := strings.TrimSuffix(filepath.Base(arg0), ".exe")
	if rest, ok := strings.CutPrefix(base, "kubectl-"); ok {
		return strings.ReplaceAll(rest, "_", "-")
	}
	return base
}

func newRootCmd(opts *globalOptions) *cobra.Command { return newRootCmdNamed(opts, os.Args[0]) }

// newRootCmdNamed builds the command tree for the binary name arg0. Help texts are written for
// "kubectl topolvm"; under another plugin name (krew's kubectl-topolvm_capacity, say) they follow it.
func newRootCmdNamed(opts *globalOptions, arg0 string) *cobra.Command {
	rootPools := &poolFlags{}
	name := commandName(arg0)
	asPlugin := strings.HasPrefix(filepath.Base(arg0), "kubectl-")
	display := name
	if asPlugin {
		display = "kubectl " + name
	}

	root := &cobra.Command{
		Use:   name,
		Short: "Show TopoLVM capacity, volumes and anomalies",
		Long: `Read-only view of TopoLVM: free and allocated space per node and device class,
which claim sits on which LogicalVolume, and checks for leaked, stuck or failed
volumes. Nothing in the cluster is changed.

Data comes from the capacity annotations topolvm-node writes on nodes, the
LogicalVolume objects and the PersistentVolumes that point at them. SIZE is
free plus allocated, not the size of the volume group: lvmd keeps spare-gb and
any LV created outside TopoLVM out of it (see --metrics).

Exit codes:
  0  success
  1  error: bad flags, API or RBAC failure, no TopoLVM nodes found
  2  check or lvs-diff found problems, a threshold was breached, a volume or a
     claim's new size does not fit, or rbac --check found a missing permission`,
		Example: `  kubectl topolvm                      # pools per node and device class
  kubectl topolvm volumes -n database  # which claim sits where
  kubectl topolvm check                # leaked, stuck or failed volumes, exit 2 if any`,
		Args:          cobra.NoArgs,
		RunE:          runPools(opts, rootPools),
		Annotations:   map[string]string{},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	if asPlugin {
		root.Annotations[cobra.CommandDisplayNameAnnotation] = display
	}

	pf := root.PersistentFlags()
	clientcmd.BindOverrideFlags(opts.overrides, pf, clientcmd.RecommendedConfigOverrideFlags(""))
	pf.Lookup("namespace").Usage = "only claims in this namespace (volumes, tree, check, pending); the claim's namespace for headroom"
	pf.StringVar(&opts.rules.ExplicitPath, "kubeconfig", "", "path to the kubeconfig file")
	pf.StringVarP(&opts.output, "output", "o", "table", "output format: table|wide|json|yaml")
	pf.BoolVar(&opts.noHeaders, "no-headers", false, "do not print table headers and total rows")
	pf.StringSliceVar(&opts.nodes, "node", nil, "only these nodes (comma-separated)")
	pf.StringSliceVar(&opts.deviceClasses, "device-class", nil, "only these device classes (comma-separated)")
	pf.StringSliceVar(&opts.thin, "thin", nil, "treat these device classes as thin pools")
	pf.StringVar(&opts.driver, "driver", "", "CSI driver: topolvm.io or topolvm.cybozu.com (default: detect)")
	pf.StringVar(&opts.topolvmNamespace, "topolvm-namespace", "topolvm-system", "namespace TopoLVM runs in")
	pf.BoolVar(&opts.bytes, "bytes", false, "print exact byte counts instead of 315.5Gi-style quantities")
	pf.StringVar(&opts.color, "color", "auto", "color cells that need attention: auto|always|never (auto honours NO_COLOR)")

	addPoolFlags(root, rootPools)
	root.AddCommand(
		newPoolsCmd(opts),
		newTreeCmd(opts),
		newVolumesCmd(opts),
		newCheckCmd(opts),
		newPendingCmd(opts),
		newFitCmd(opts),
		newHeadroomCmd(opts),
		newNodeCmd(opts),
		newRBACCmd(opts),
		newDumpCmd(opts),
		newLVSDiffCmd(opts),
		newVersionCmd(opts),
	)
	if asPlugin && name != "topolvm" {
		renameHelp(root, "kubectl "+name)
	}
	return root
}

func renameHelp(c *cobra.Command, display string) {
	c.Long = strings.ReplaceAll(c.Long, "kubectl topolvm", display)
	c.Example = strings.ReplaceAll(c.Example, "kubectl topolvm", display)
	for _, sub := range c.Commands() {
		renameHelp(sub, display)
	}
}

// validateOutput ensures the -o value is one we support.
func (o *globalOptions) validateOutput() error {
	switch o.output {
	case "table", "wide", "json", "yaml":
		return nil
	default:
		return fmt.Errorf("invalid output format %q: want table, wide, json or yaml", o.output)
	}
}

func (o *globalOptions) validateColor() error {
	switch o.color {
	case "auto", "always", "never":
		return nil
	default:
		return fmt.Errorf("invalid --color %q: want auto, always or never", o.color)
	}
}
