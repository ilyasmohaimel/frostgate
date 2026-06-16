// Command frostgate probes self-hosted endpoints and ranks them by latency.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
	"github.com/ilyasmohaimel/frostgate/internal/report"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfgPath := flag.String("config", "frostgate.yaml", "path to config file")
	jsonOut := flag.Bool("json", false, "emit machine-readable JSON instead of a table")
	watch := flag.Bool("watch", false, "re-probe continuously")
	interval := flag.Duration("interval", 5*time.Second, "delay between probes in --watch mode")
	timeout := flag.Duration("timeout", 0, "override the per-probe timeout (e.g. 3s)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "frostgate:", err)
		return 3
	}
	if *timeout > 0 {
		cfg.Defaults.Timeout.Duration = *timeout
	}

	if *watch {
		watchLoop(cfg, *jsonOut, *interval)
		return 0
	}

	results := probe.Run(context.Background(), cfg)
	emit(results, *jsonOut)
	return exitCode(results)
}

// emit prints one round of results in the chosen format.
func emit(results []probe.Result, asJSON bool) {
	if asJSON {
		if err := report.JSON(os.Stdout, results); err != nil {
			fmt.Fprintln(os.Stderr, "frostgate:", err)
		}
		return
	}
	fmt.Println(report.Table(results))
}

// watchLoop re-probes on an interval, clearing the screen each round.
func watchLoop(cfg *config.Config, asJSON bool, interval time.Duration) {
	for {
		fmt.Print("\033[H\033[2J") // move cursor home + clear screen
		fmt.Printf("frostgate — %s\n\n", time.Now().Format("15:04:05"))
		results := probe.Run(context.Background(), cfg)
		emit(results, asJSON)
		time.Sleep(interval)
	}
}

// exitCode maps overall health to a shell-friendly status code.
//
//	0 = every route up   1 = some routes down   2 = all routes down
func exitCode(results []probe.Result) int {
	up, down := 0, 0
	for _, r := range results {
		if r.OK() {
			up++
		} else {
			down++
		}
	}
	switch {
	case down == 0:
		return 0
	case up == 0:
		return 2
	default:
		return 1
	}
}
