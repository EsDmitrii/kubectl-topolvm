// Command kubectl-topolvm is a read-only kubectl plugin that shows TopoLVM capacity per node and
// device class, maps LogicalVolumes to PersistentVolumes and claims, and checks for anomalies.
package main

import (
	"os"

	"github.com/EsDmitrii/kubectl-topolvm/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
