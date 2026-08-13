// Command backup-tower snapshots container volumes and configuration, and
// updates containers with a tested way back.
package main

import (
	"os"

	"github.com/Hudint/backup-tower/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
