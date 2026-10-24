package cli

import (
	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

func newNodeCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "node NODE",
		Short: "Show what a node holds, what deleting it removes and whether its volumes fit elsewhere",
		Long: `TopoLVM volumes live on one node and never move, so a node is a unit of risk.
This shows the node's pools, every volume with the workload that mounts it and
whether that workload has a copy with its own volume on another node.

Deleting the Node object makes the TopoLVM controller delete every claim of a
TopoLVM StorageClass whose selected-node is that node, Retain StorageClasses
included, then every LogicalVolume of the node. PVs that outlive their claim or
have none stay without an LV. A controller with one replica on that very node
does this only after its pod is evicted, 5 minutes by default, and one started
with --skip-node-finalize does none of it.

Those lists are printed before an evacuation estimate: each volume, largest
first, on the first other Ready, schedulable node of the same device class with
room left.`,
		Example: `  kubectl topolvm node worker-3
  kubectl topolvm node worker-3 -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := opts.collect(cmd, kube.CollectOptions{
				PVs:   kube.PVRequired,
				Needs: kube.NeedPVCs | kube.NeedPods | kube.NeedNodeStates | kube.NeedStorage,
			})
			if err != nil {
				return err
			}
			nr, err := report.BuildNode(in, args[0])
			if err != nil {
				return err
			}
			r := report.Build(in, report.Options{Nodes: args[:1], SizeNotes: true})
			if werr := opts.warn(cmd, r.Warnings); werr != nil {
				return werr
			}
			return opts.emit(cmd, output.Wrap(r, "node", nr), func() error {
				return output.WriteNode(cmd.OutOrStdout(), nr, r.Driver, opts.tableOptions(cmd))
			})
		},
	}
}
