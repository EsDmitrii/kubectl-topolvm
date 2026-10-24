package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

func newVolumesCmd(opts *globalOptions) *cobra.Command {
	var (
		sortBy, groupBy string
		usage           bool
	)
	cmd := &cobra.Command{
		Use:     "volumes",
		Aliases: []string{"vol", "lv"},
		Short:   "List LogicalVolumes with their node, claim and status",
		Long: `One row per LogicalVolume, joined to its PersistentVolume by volumeHandle.

STATUS is Error, Deleting, Resizing, Snapshot, the phase of the PV (Bound,
Released, ...) or Orphan when no PV points at the volume. -o wide adds the
volume ID, which is the LV name lvs prints on the node.

All namespaces are listed unless -n is given. --group-by sums volumes per
namespace, StorageClass, node or device class.

--usage adds what the filesystem inside each volume uses, from kubelet. It needs
get nodes/proxy, which is close to exec on the node; see README "RBAC". The
figures are a snapshot, and a claim cannot be shrunk.`,
		Example: `  kubectl topolvm volumes
  kubectl topolvm volumes -n database -o wide
  kubectl topolvm volumes --group-by namespace
  kubectl topolvm volumes --usage --sort-by idle`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if groupBy != "" && !slices.Contains(report.GroupKeys, groupBy) {
				return fmt.Errorf("unknown --group-by %q; want one of %s", groupBy, strings.Join(report.GroupKeys, ", "))
			}
			if err := output.CheckVolumeSortKey(sortBy, usage); err != nil {
				return err
			}
			req := kube.CollectOptions{PVs: kube.PVOptional}
			if usage {
				req.Needs |= kube.NeedUsage
			}
			r, err := opts.build(cmd, req, nil)
			if err != nil {
				return err
			}
			if err := output.SortVolumes(&r, sortBy); err != nil {
				return err
			}
			o := opts.tableOptions(cmd)
			o.Usage = usage
			if usage {
				if err := opts.note(cmd, "--usage is a snapshot from kubelet; PVCs cannot be shrunk"); err != nil {
					return err
				}
			}
			w := cmd.OutOrStdout()
			if groupBy != "" {
				groups := report.GroupVolumes(r.Volumes, groupBy)
				return opts.emit(cmd, output.Wrap(r, "groups", groups), func() error { return output.WriteGroups(w, groups, groupBy, o) })
			}
			return opts.emit(cmd, mustView(r, output.SectionVolumes), func() error { return output.WriteVolumes(w, r, o) })
		},
	}
	cmd.Flags().StringVar(&sortBy, "sort-by", "", "sort rows by node|namespace|pvc|size|age|status (with --usage also fs-used|idle)")
	cmd.Flags().StringVar(&groupBy, "group-by", "", "sum volumes by namespace|storageclass|node|device-class")
	cmd.Flags().BoolVar(&usage, "usage", false, "add filesystem usage from kubelet (needs get nodes/proxy)")
	return cmd
}
