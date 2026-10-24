package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/EsDmitrii/kubectl-topolvm/internal/report"
	"github.com/EsDmitrii/kubectl-topolvm/internal/version"
)

func newVersionCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.validateOutput(); err != nil {
				return err
			}
			info := map[string]string{"schemaVersion": report.SchemaVersion, "version": version.Version, "commit": version.Commit, "date": version.Date}
			w := cmd.OutOrStdout()
			switch opts.output {
			case "json":
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			case "yaml":
				out, err := yaml.Marshal(info)
				if err != nil {
					return err
				}
				_, err = w.Write(out)
				return err
			}
			_, err := fmt.Fprintf(w, "kubectl-topolvm %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
			return err
		},
	}
}
