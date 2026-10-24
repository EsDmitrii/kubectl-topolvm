package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

// poolFlags belong to the pools view, which is both the root command and the pools subcommand.
type poolFlags struct {
	sortBy   string
	failOver float64
	minFree  string
	metrics  bool
}

func addPoolFlags(cmd *cobra.Command, pf *poolFlags) {
	cmd.Flags().StringVar(&pf.sortBy, "sort-by", "", "sort rows by node|device-class|size|used|free|use%|lvs")
	cmd.Flags().Float64Var(&pf.failOver, "fail-over", 0, "exit 2 when a pool is more than this percent used (thin pools: data use, with --metrics)")
	cmd.Flags().StringVar(&pf.minFree, "min-free", "", "exit 2 when a pool has less free space than this, e.g. 100Gi")
	cmd.Flags().BoolVar(&pf.metrics, "metrics", false,
		"read VG size and thin pool fill from topolvm-node metrics (needs get pods/proxy in the TopoLVM namespace)")
}

func runPools(opts *globalOptions, pf *poolFlags) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if pf.sortBy != "" && !slices.Contains(output.PoolSortKeys, pf.sortBy) {
			return fmt.Errorf("unknown sort key %q; want one of %s", pf.sortBy, strings.Join(output.PoolSortKeys, ", "))
		}
		if pf.failOver < 0 || pf.failOver > 100 {
			return errors.New("--fail-over must be a percentage from 0 (off) to 100")
		}
		var minFree int64
		if pf.minFree != "" {
			v, err := parseSize(pf.minFree)
			if err != nil {
				return fmt.Errorf("--min-free: %w", err)
			}
			minFree = v
		}
		req := kube.CollectOptions{PVs: kube.PVSkip}
		if pf.metrics {
			req.Needs |= kube.NeedTopoLVMPods | kube.NeedMetrics
		}
		r, err := opts.build(cmd, req, func(o *report.Options) { o.SizeNotes = true })
		if err != nil {
			return err
		}
		if serr := output.SortPools(&r, pf.sortBy); serr != nil {
			return serr
		}
		r.Breaches = report.Breaches(r, pf.failOver, minFree)
		gaps := report.FailOverGaps(r, pf.failOver)
		r.Warnings = append(r.Warnings, gaps...)
		if werr := opts.warn(cmd, gaps); werr != nil {
			return werr
		}
		o := opts.tableOptions(cmd)
		o.Metrics = pf.metrics
		err = opts.emit(cmd, mustView(r, output.SectionPools), func() error { return output.WritePools(cmd.OutOrStdout(), r, o) })
		for _, b := range r.Breaches {
			if err == nil {
				err = opts.note(cmd, "breach: %s", b.Message)
			}
		}
		if err == nil && len(r.Breaches) > 0 {
			err = errFindings
		}
		return err
	}
}

func mustView(r report.Report, s output.Section) any {
	v, err := output.View(r, s)
	if err != nil {
		panic(err)
	}
	return v
}

func newPoolsCmd(opts *globalOptions) *cobra.Command {
	pf := &poolFlags{}
	cmd := &cobra.Command{
		Use:   "pools",
		Short: "Show free and allocated space per node and device class (default command)",
		Long: `One row per node and device class. SIZE is FREE plus USED, where FREE is what
topolvm-node reports in the capacity annotation and USED is the sum of the
LogicalVolumes on that node. The * rows sum a class over its nodes.

For thin classes SIZE, USED and FREE are virtual sizes and USE% is not shown.

--fail-over and --min-free turn the table into a check for cron and CI: every
pool over the threshold is printed as a breach on stderr and the exit code is 2.`,
		Example: `  kubectl topolvm
  kubectl topolvm pools --device-class ssd -o wide --metrics
  kubectl topolvm --fail-over 85 --min-free 50Gi || alert`,
		Args: cobra.NoArgs,
		RunE: runPools(opts, pf),
	}
	addPoolFlags(cmd, pf)
	return cmd
}
