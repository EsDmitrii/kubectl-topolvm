package cli

import (
	"sort"

	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

func newTreeCmd(opts *globalOptions) *cobra.Command {
	var ascii bool
	cmd := &cobra.Command{
		Use:   "tree",
		Short: "Show node → device class → claim → pod",
		Long: `One tree per node: its device classes with their pools, the claims on each class
and the pods that mount them. LogicalVolumes without a claim show as <orphan>
or <snapshot>.`,
		Example: `  kubectl topolvm tree
  kubectl topolvm tree --node worker-3 --ascii`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, err := opts.collect(cmd, kube.CollectOptions{PVs: kube.PVOptional, Needs: kube.NeedPods})
			if err != nil {
				return err
			}
			ro := opts.reportOptions(cmd)
			ro.SizeNotes = true
			r := report.Build(in, ro)
			if werr := opts.warn(cmd, r.Warnings); werr != nil {
				return werr
			}
			pods := podsByClaim(in)
			o := opts.tableOptions(cmd)
			o.ASCII = ascii
			return opts.emit(cmd, output.Wrap(r, "tree", map[string]any{"pools": r.Pools, "volumes": r.Volumes, "pods": pods}),
				func() error { return output.WriteTree(cmd.OutOrStdout(), r, pods, o) })
		},
	}
	cmd.Flags().BoolVar(&ascii, "ascii", false, "draw the tree with plain ASCII characters")
	return cmd
}

// podsByClaim lists the pods that mount each claim, by namespace/claim.
func podsByClaim(in report.Input) map[string][]output.TreePod {
	out := map[string][]output.TreePod{}
	for i := range in.Pods {
		p := &in.Pods[i]
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				key := p.Namespace + "/" + v.PersistentVolumeClaim.ClaimName
				out[key] = append(out[key], output.TreePod{Name: p.Name, Phase: string(p.Status.Phase)})
			}
		}
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool { return out[k][i].Name < out[k][j].Name })
	}
	return out
}
