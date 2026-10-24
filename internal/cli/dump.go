package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/EsDmitrii/kubectl-topolvm/internal/dump"
	"github.com/EsDmitrii/kubectl-topolvm/internal/kube"
)

func newDumpCmd(opts *globalOptions) *cobra.Command {
	var (
		dir       string
		anonymize bool
	)
	cmd := &cobra.Command{
		Use:   "dump",
		Short: "Write what the plugin reads to a directory, for bug reports and tests",
		Long: `Writes nodes, LogicalVolumes, PersistentVolumes, claims, pods, events, storage
objects and the lvmd device classes in the layout of the plugin's test fixtures.
Objects are cut down to the fields the plugin reads: pod environments, images
and unrelated annotations are left out.

--anonymize also renames nodes, namespaces, claims, pods, owners, StorageClasses,
non-generated volume names, finalizers and taint keys that are not Kubernetes' own,
including where names appear in event messages, and masks IPv4 addresses there.
Not renamed: device classes, volume groups and the lvmd config, generated
pvc-<uid> names and UIDs, the TopoLVM namespace, sizes and timestamps. Review the
files before sharing them.`,
		Example: `  kubectl topolvm dump --dir ./topolvm-dump --anonymize`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				return errors.New("--dir is required")
			}
			in, err := opts.collect(cmd, kube.CollectOptions{
				PVs:   kube.PVOptional,
				Needs: kube.NeedPVCs | kube.NeedPods | kube.NeedEvents | kube.NeedStorage | kube.NeedNodeStates | kube.NeedTopoLVMPods,
			})
			if err != nil {
				return err
			}
			if err := dump.Write(dir, in, anonymize); err != nil {
				return err
			}
			if !anonymize {
				if err := opts.note(cmd, "names are not anonymized; pass --anonymize before sharing the dump"); err != nil {
					return err
				}
			}
			return opts.note(cmd, "wrote %s", dir)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to write, must not exist or be empty")
	cmd.Flags().BoolVar(&anonymize, "anonymize", false, "rename nodes, namespaces, claims, pods, owners and other site names")
	return cmd
}
