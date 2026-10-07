// Command trading-agent runs backtests, scans and paper-trading loops.
package main

import (
	"os"

	"github.com/rdone44/trading-agent-go/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
