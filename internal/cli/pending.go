package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

func newPendingCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "pending",
		Short: "Explain why TopoLVM claims are still Pending",
		Long: `Lists Pending claims of TopoLVM StorageClasses with a reason:

  Immediate      the StorageClass binds immediately; TopoLVM recommends WaitForFirstConsumer
  NoConsumer     no pod uses the claim yet, which is normal for WaitForFirstConsumer
  Unschedulable  a pod waits but the scheduler picked no node; compares the request
                 with the largest free space in the device class
  Provisioning   a node was picked; compares the request with its free space

-o wide adds the last FailedScheduling or ProvisioningFailed event. Events expire
after about an hour, so the computed reason is the one to trust.`,
		Example: `  kubectl topolvm pending
  kubectl topolvm pending -n database -o wide`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, err := opts.collect(cmd, kube.CollectOptions{Needs: kube.NeedPVCs | kube.NeedPods | kube.NeedEvents | kube.NeedStorage})
			if err != nil {
				return err
			}
			r := report.Build(in, report.Options{})
			if werr := opts.warn(cmd, r.Warnings); werr != nil {
				return werr
			}
			if in.PVCs == nil || in.StorageClasses == nil {
				return fmt.Errorf("pending needs persistentvolumeclaims and storageclasses: %s", strings.Join(r.Warnings, "; "))
			}
			rows := report.BuildPending(in, opts.reportOptions(cmd))
			return opts.emit(cmd, output.Wrap(r, "pending", rows), func() error {
				return output.WritePending(cmd.OutOrStdout(), rows, r.GeneratedAt, opts.tableOptions(cmd))
			})
		},
	}
}
