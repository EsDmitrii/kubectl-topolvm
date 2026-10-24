package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/units"
)

func newHeadroomCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "headroom [NAMESPACE/]PVC [SIZE]",
		Short: "Show how far a claim can grow on its node; with SIZE exit 2 if it cannot",
		Long: `Follows the claim to its PersistentVolume, LogicalVolume and node and compares
the growth with the free space of its device class there. TopoLVM volumes never
move, so a claim can only grow as far as its own node allows.

SIZE is the new total size of the claim. Exit 2 when it does not fit, when the
free space on the node is unknown, or when the StorageClass does not allow
expansion; a SIZE below the current size is an error, claims cannot shrink.

Also shows whether the StorageClass allows expansion, pvc-autoresizer
annotations (resize.topolvm.io/*) and the last VolumeResizeFailed event.`,
		Example: `  kubectl topolvm headroom database/data-pg-0
  kubectl topolvm headroom -n database data-pg-0 200Gi`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, name, found := strings.Cut(args[0], "/")
			if !found {
				ns, name = opts.namespace(cmd), args[0]
			}
			var size *int64
			if len(args) == 2 {
				v, err := parseSize(args[1])
				if err != nil {
					return err
				}
				size = &v
			}
			in, err := opts.collect(cmd, kube.CollectOptions{PVs: kube.PVRequired, Needs: kube.NeedPVCs | kube.NeedEvents | kube.NeedStorage})
			if err != nil {
				return err
			}
			h, err := report.BuildHeadroom(in, ns, name, size)
			if err != nil {
				return err
			}
			if size != nil && *size < h.CurrentBytes {
				return fmt.Errorf("%s is below the current %s: a PVC cannot shrink", units.FormatIEC(*size), units.FormatIEC(h.CurrentBytes))
			}
			r := report.Build(in, report.Options{})
			if werr := opts.warn(cmd, r.Warnings); werr != nil {
				return werr
			}
			err = opts.emit(cmd, output.Wrap(r, "headroom", h), func() error { return output.WriteHeadroom(cmd.OutOrStdout(), h, opts.tableOptions(cmd)) })
			if err == nil && size != nil && (h.Fits == nil || !*h.Fits || !h.AllowExpansion) {
				err = errFindings
			}
			return err
		},
	}
}
