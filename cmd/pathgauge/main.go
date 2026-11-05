// Command pathgauge is the single binary for every PathGauge role:
// server, agent, standalone endpoint, CLI client and host diagnostics.
package main

import (
	"os"

	"github.com/agoodley/pathgauge/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
