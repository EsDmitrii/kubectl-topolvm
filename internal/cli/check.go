package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

func newCheckCmd(opts *globalOptions) *cobra.Command {
	var (
		minAge time.Duration
		skip   []string
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Find leaked, stuck and failed volumes; exit 2 when something is found",
		Long: `Runs read-only checks over LogicalVolumes, PersistentVolumes and nodes and prints
one row per problem. Nothing is fixed or deleted: each row says what to look at.

Checks:
  orphan-lv           LogicalVolume without a PersistentVolume (snapshots excluded)
  pv-without-lv       TopoLVM PersistentVolume without a LogicalVolume
  released-pv         Released PersistentVolume whose LV still holds the space
  lv-on-missing-node  LogicalVolume on a node that no longer exists
  stuck-deletion      LogicalVolume deleted longer than --min-age ago
  pending-deletion    pendingdeletion annotation set while the finalizer stays
  lv-error            status.code set by lvmd: failed create or expand
  resize-pending      status.currentSize below spec.size longer than --min-age
  uid-mismatch        status.volumeID differs from metadata.uid
  storage-capacity-disabled  CSIDriver storageCapacity is not true, the scheduler ignores capacity
  immediate-binding   TopoLVM StorageClass without WaitForFirstConsumer
  no-default-class    StorageClass without a device class while no node has a default one
  capacity-missing    no CSIStorageCapacity for a node and StorageClass older than 2m (topolvm#1201)
  capacity-mismatch   CSIStorageCapacity above the node annotation, so the scheduler overestimates room
  stale-class-annotation  capacity annotation of a class the lvmd config no longer has
  node-without-capacity   node registered with the driver but without capacity annotations
  topolvm-node-not-ready  no ready topolvm-node pod on a TopoLVM node
  pvc-pending         claim Pending longer than --min-age for a reason other than NoConsumer
  pvc-resize-stuck    claim requests more than its capacity longer than --min-age

Exit code 2 when anything is found, so it fits cron and CI.`,
		Example: `  kubectl topolvm check
  kubectl topolvm check --skip released-pv -o json
  kubectl topolvm check --min-age 1h || alert`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, id := range skip {
				if !slices.Contains(report.CheckIDs, id) {
					return fmt.Errorf("unknown check %q; known: %s", id, strings.Join(report.CheckIDs, ", "))
				}
			}
			r, err := opts.build(cmd, kube.CollectOptions{
				PVs:   kube.PVRequired,
				Needs: kube.NeedPVCs | kube.NeedPods | kube.NeedEvents | kube.NeedStorage | kube.NeedTopoLVMPods,
			}, func(ro *report.Options) {
				ro.MinAge, ro.Skip, ro.Checks = minAge, skip, true
			})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			switch {
			case opts.structured():
				err = output.Structured(w, r, output.SectionCheck, opts.output)
			case len(r.Findings) == 0:
				_, err = fmt.Fprintln(cmd.ErrOrStderr(), "no findings")
			default:
				err = output.WriteFindings(w, r, opts.tableOptions(cmd))
			}
			if err == nil && len(r.Findings) > 0 {
				err = errFindings
			}
			return err
		},
	}
	cmd.Flags().DurationVar(&minAge, "min-age", defaultMinAge, "ignore states younger than this (provisioning and resizing take a moment)")
	cmd.Flags().StringSliceVar(&skip, "skip", nil, "checks to skip (comma-separated IDs)")
	return cmd
}
