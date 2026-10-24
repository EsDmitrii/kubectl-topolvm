package cli

import (
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
	"github.com/EsDmitrii/kubectl-topolvm/internal/output"
	"github.com/EsDmitrii/kubectl-topolvm/internal/rbac"
	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
)

// clientsFn builds API clients for commands that call the API directly. Tests swap it.
var clientsFn = func(o *globalOptions) (*kube.Clients, error) {
	cfg, err := kube.RESTConfig(o.rules, o.overrides)
	if err != nil {
		return nil, err
	}
	return kube.NewClients(cfg)
}

func newRBACCmd(opts *globalOptions) *cobra.Command {
	var (
		with  []string
		check bool
	)
	cmd := &cobra.Command{
		Use:   "rbac",
		Short: "Print the ClusterRole and Role the plugin needs, or check them for the current user",
		Long: `Without flags prints a read-only ClusterRole for every command and a Role in the
TopoLVM namespace (lvmd ConfigMaps and topolvm-node pods). --with adds the
rights of optional modes:

  metrics  get pods/proxy in the TopoLVM namespace, for --metrics
  usage    get nodes/proxy, for volumes --usage; this reaches the kubelet API
           and is close to exec on the node, grant it with care
  lvms     list lvmvolumegroups.lvm.topolvm.io, for Red Hat LVMS

--check asks the API server whether you hold each permission
(SelfSubjectAccessReview, the same call as kubectl auth can-i) and exits 2 when
one is missing.`,
		Example: `  kubectl topolvm rbac | kubectl apply -f -
  kubectl topolvm rbac --with metrics,lvms --topolvm-namespace openshift-storage
  kubectl topolvm rbac --check`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !check {
				if opts.output != "" && opts.output != "table" && opts.output != "yaml" {
					return errors.New("rbac prints YAML manifests; -o json and yaml apply to --check")
				}
				y, err := rbac.YAML(with, opts.topolvmNamespace)
				if err != nil {
					return err
				}
				_, err = io.WriteString(cmd.OutOrStdout(), y)
				return err
			}
			if err := opts.validateOutput(); err != nil {
				return err
			}
			c, err := clientsFn(opts)
			if err != nil {
				return err
			}
			results, err := rbac.Check(cmd.Context(), c.Core, with, opts.topolvmNamespace)
			if err != nil {
				return err
			}
			doc := map[string]any{"schemaVersion": report.SchemaVersion, "permissions": results, "warnings": []string{}}
			err = opts.emit(cmd, doc, func() error {
				return output.WritePermissions(cmd.OutOrStdout(), results, opts.tableOptions(cmd))
			})
			for _, r := range results {
				if err == nil && !r.Allowed {
					err = errFindings
				}
			}
			return err
		},
	}
	cmd.Flags().StringSliceVar(&with, "with", nil, "add rights for optional modes: metrics, usage, lvms")
	cmd.Flags().BoolVar(&check, "check", false, "check the permissions of the current user instead of printing roles")
	return cmd
}
