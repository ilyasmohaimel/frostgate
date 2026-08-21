package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/doctor"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
	"github.com/ilyasmohaimel/frostgate/internal/report"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
	"github.com/ilyasmohaimel/frostgate/internal/tui"
)

var version = "dev"

func init() {
	if version == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
	}
}

func main() {
	os.Exit(run())
}

func run() int {
	// Support subcommand: frostgate setup
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		home := setup.DefaultHomeDir()
		// Allow custom HOME for testing via env
		if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
			home = h
		}
		if err := setup.RunInteractive(setup.Options{HomeDir: home}); err != nil {
			fmt.Fprintln(os.Stderr, "setup:", err)
			return 3
		}
		return 0
	}
	// Support subcommand: frostgate doctor
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		// allow --config after subcommand
		fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
		cfgPath := fs.String("config", "", "path to config file")
		verbose := fs.Bool("verbose", false, "verbose output")
		_ = fs.Parse(os.Args[2:])
		path := config.ResolveConfigPath(*cfgPath)
		checks := doctor.Run(path, *verbose)
		fmt.Print(doctor.FormatColor(checks))
		// exit 0 if all PASS/WARN, 1 if any FAIL
		for _, c := range checks {
			if c.Status == "FAIL" {
				return 1
			}
		}
		return 0
	}

	cfgPath := flag.String("config", "", "path to config file (default: $XDG_CONFIG_HOME/frostgate/config.yaml, ~/.config/frostgate/config.yaml, ./frostgate.yaml)")
	jsonOut := flag.Bool("json", false, "emit machine-readable JSON")
	watch := flag.Bool("watch", false, "re-probe continuously with TUI (or NDJSON if --json)")
	interval := flag.Duration("interval", 5*time.Second, "interval between probes in --watch")
	timeout := flag.Duration("timeout", 0, "override per-probe timeout (e.g. 3s)")
	doctorFlag := flag.Bool("doctor", false, "run diagnostics and exit")
	checkFlag := flag.Bool("check", false, "validate config and exit")
	versionFlag := flag.Bool("version", false, "print version and exit")
	verbose := flag.Bool("verbose", false, "verbose/debug output")
	debug := flag.Bool("debug", false, "debug output (alias for --verbose)")
	setupFlag := flag.Bool("setup", false, "run interactive setup wizard")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "frostgate %s — Network Route Sentinel\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage:\n  frostgate [flags]\n  frostgate setup\n  frostgate doctor [--config path]\n\n")
		fmt.Fprintf(os.Stderr, "Commands:\n  setup   interactive setup wizard (recommended first run)\n  doctor  run diagnostics\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExit codes: 0 all up  1 mixed/degraded  2 all down  3 config/error\n")
		fmt.Fprintf(os.Stderr, "\nConfig discovery (first existing wins):\n  1. --config path\n  2. $XDG_CONFIG_HOME/frostgate/config.yaml\n  3. ~/.config/frostgate/config.yaml\n  4. ./frostgate.yaml\n")
	}

	flag.Parse()

	if *verbose || *debug {
		*verbose = true
	}

	if *versionFlag {
		fmt.Printf("frostgate %s\n", version)
		return 0
	}
	if *setupFlag {
		home := setup.DefaultHomeDir()
		if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
			home = h
		}
		if err := setup.RunInteractive(setup.Options{HomeDir: home}); err != nil {
			fmt.Fprintln(os.Stderr, "setup:", err)
			return 3
		}
		return 0
	}
	if *doctorFlag {
		path := config.ResolveConfigPath(*cfgPath)
		checks := doctor.Run(path, *verbose)
		fmt.Print(doctor.FormatColor(checks))
		for _, c := range checks {
			if c.Status == "FAIL" {
				return 1
			}
		}
		return 0
	}

	resolved := config.ResolveConfigPath(*cfgPath)
	if *checkFlag {
		if _, err := config.Load(resolved); err != nil {
			fmt.Fprintf(os.Stderr, "config check FAIL: %v\n", err)
			if *verbose {
				fmt.Fprintf(os.Stderr, "path: %s\n", resolved)
			}
			return 3
		}
		fmt.Printf("config OK: %s\n", resolved)
		return 0
	}

	cfg, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintln(os.Stderr, "frostgate:", err)
		if *verbose {
			fmt.Fprintf(os.Stderr, "config path: %s\n", resolved)
		}
		return 3
	}
	if *timeout > 0 {
		cfg.Defaults.Timeout.Duration = *timeout
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *watch {
		if *jsonOut {
			return watchJSON(ctx, cfg, *interval, *verbose)
		}
		return watchTUI(ctx, cfg, *interval, *verbose)
	}

	results := probe.Run(ctx, cfg)
	emit(results, *jsonOut, *verbose)
	return exitCode(results)
}

func emit(results []probe.Result, asJSON bool, verbose bool) {
	if asJSON {
		if err := report.JSON(os.Stdout, results); err != nil {
			fmt.Fprintln(os.Stderr, "frostgate:", err)
		}
		return
	}
	fmt.Println(report.Table(results))
	if verbose {
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "debug %s: %v (category=%s)\n", r.Endpoint.Name, r.Err, r.Category)
			}
		}
	}
}

func watchJSON(ctx context.Context, cfg *config.Config, interval time.Duration, verbose bool) int {
	for {
		select {
		case <-ctx.Done():
			return 0
		default:
		}
		results := probe.Run(ctx, cfg)
		line, err := report.NDJSONLine(results)
		if err != nil {
			fmt.Fprintln(os.Stderr, "json encode:", err)
		} else {
			fmt.Println(string(line))
		}
		if verbose {
			for _, r := range results {
				if r.Err != nil {
					fmt.Fprintf(os.Stderr, "debug %s: %v\n", r.Endpoint.Name, r.Err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(interval):
		}
	}
}

func watchTUI(ctx context.Context, cfg *config.Config, interval time.Duration, verbose bool) int {
	m := tui.New(cfg, interval, verbose)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		if strings.Contains(err.Error(), "context canceled") {
			return 0
		}
		fmt.Fprintln(os.Stderr, "tui:", err)
		return 3
	}
	return 0
}

func exitCode(results []probe.Result) int {
	if len(results) == 0 {
		return 3
	}
	up, degraded, down := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case probe.StatusUp:
			up++
		case probe.StatusDegraded:
			degraded++
		default:
			down++
		}
	}
	if down == len(results) {
		return 2
	}
	if up == len(results) {
		return 0
	}
	// mixed or any degraded/down
	if degraded > 0 || down > 0 {
		return 1
	}
	return 0
}
