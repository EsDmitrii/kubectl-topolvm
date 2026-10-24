package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

func newFitCmd(opts *globalOptions) *cobra.Command {
	var (
		storageClass string
		replicas     int
	)
	cmd := &cobra.Command{
		Use:   "fit SIZE",
		Short: "Show which nodes can host a new volume of SIZE; exit 2 if too few",
		Long: `Compares SIZE with the free space of the device class on every node. The class
comes from --storage-class (its topolvm.io/device-class parameter), else
--device-class, else the default class. Cordoned, NotReady and tainted nodes are
listed with a note. Cordoned and NotReady nodes do not count towards --replicas;
tainted ones do, since tolerations are not checked. --replicas assumes one
replica per node, as anti-affinity spreads a StatefulSet.

This is an estimate: the scheduler checks each claim on its own, does not
reserve space between claims and LVs are rounded up to whole extents.`,
		Example: `  kubectl topolvm fit 100Gi
  kubectl topolvm fit 50Gi --storage-class topolvm-fast --replicas 3`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			size, err := parseSize(args[0])
			if err != nil {
				return err
			}
			if len(opts.deviceClasses) > 1 {
				return errors.New("fit takes a single --device-class")
			}
			if replicas < 1 {
				return errors.New("--replicas must be at least 1")
			}
			fo := report.FitOptions{Size: size, StorageClass: storageClass, Replicas: replicas, Nodes: opts.nodes}
			if len(opts.deviceClasses) == 1 {
				fo.DeviceClass = opts.deviceClasses[0]
			}
			in, err := opts.collect(cmd, kube.CollectOptions{Needs: kube.NeedStorage | kube.NeedNodeStates})
			if err != nil {
				return err
			}
			res, err := report.BuildFit(in, fo)
			if err != nil {
				return err
			}
			r := report.Build(in, report.Options{})
			if werr := opts.warn(cmd, r.Warnings); werr != nil {
				return werr
			}
			err = opts.emit(cmd, output.Wrap(r, "fit", res), func() error {
				return output.WriteFit(cmd.OutOrStdout(), res, opts.tableOptions(cmd))
			})
			if err == nil {
				err = opts.note(cmd, "fits on %d of %d nodes (want %d); the scheduler checks each claim on its own, treat this as an estimate",
					res.NodesThatFit, len(res.Rows), res.Replicas)
			}
			if err == nil && !res.OK {
				err = errFindings
			}
			return err
		},
	}
	cmd.Flags().StringVar(&storageClass, "storage-class", "", "StorageClass the volume would use")
	cmd.Flags().IntVar(&replicas, "replicas", 1, "number of volumes that must fit on distinct nodes")
	return cmd
}
