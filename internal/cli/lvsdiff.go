package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/lvsdiff"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

func newLVSDiffCmd(opts *globalOptions) *cobra.Command {
	var (
		node, file string
		all        bool
	)
	cmd := &cobra.Command{
		Use:   "lvs-diff --node NODE [-f FILE]",
		Short: "Compare the LogicalVolumes of a node with lvs output taken on it",
		Long: `Finds LVs that exist on disk but not in the API (leaked space) and LogicalVolumes
whose LV is gone. The plugin runs nothing on the node: capture lvs yourself,
for example with

  ` + lvsdiff.Command("NODE") + `

from a terminal (kubectl ends the session when its stdin closes; the output also
stays in the pod log, kubectl logs node-debugger-...), then delete the debug
pod (kubectl get pods shows it as node-debugger-...) and pass the file with -f,
or the output on stdin.

States: other-node (the API places the LV on another node, so the lvs output
most likely comes from there), disk-only, cr-only and size-mismatch need
attention and exit 2; ok matches; foreign LVs (other volume groups, names that
are not volume IDs, such as the root LV or a thin pool) show only with --all.`,
		Example: `  kubectl topolvm lvs-diff --node worker-3 -f lvs-worker-3.json`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if node == "" {
				return errors.New("--node is required")
			}
			in := cmd.InOrStdin()
			if f, ok := in.(*os.File); ok && (file == "" || file == "-") && term.IsTerminal(int(f.Fd())) {
				return errors.New("lvs-diff reads lvs JSON: pass -f FILE or pipe the output in")
			}
			if file != "" && file != "-" {
				f, err := os.Open(file)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				in = f
			}
			state, err := opts.collect(cmd, kube.CollectOptions{PVs: kube.PVOptional})
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(state.Nodes, func(n topolvm.Node) bool { return n.Name == node }) {
				return fmt.Errorf("node %q is not a TopoLVM node", node)
			}
			rows, err := lvsdiff.Diff(state, node, in, all)
			if err != nil {
				return err
			}
			r := report.Build(state, report.Options{Nodes: []string{node}})
			r.Warnings = append(r.Warnings, lvsdiff.Warnings(rows, node)...)
			if werr := opts.warn(cmd, r.Warnings); werr != nil {
				return werr
			}
			err = opts.emit(cmd, output.Wrap(r, "lvs", rows), func() error {
				return output.WriteLVSDiff(cmd.OutOrStdout(), rows, opts.tableOptions(cmd))
			})
			if err == nil && lvsdiff.Problems(rows) {
				err = errFindings
			}
			return err
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "node the lvs output comes from")
	cmd.Flags().StringVarP(&file, "file", "f", "-", "lvs JSON output, - for stdin")
	cmd.Flags().BoolVar(&all, "all", false, "also show LVs that are not TopoLVM volumes")
	return cmd
}
