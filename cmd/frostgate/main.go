// Command frostgate probes self-hosted endpoints and ranks them by latency.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ilyasmohaimel/frostgate/internal/benchmark"
	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/diagnose"
	"github.com/ilyasmohaimel/frostgate/internal/doctor"
	"github.com/ilyasmohaimel/frostgate/internal/history"
	"github.com/ilyasmohaimel/frostgate/internal/importer"
	"github.com/ilyasmohaimel/frostgate/internal/lockfile"
	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/network"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
	"github.com/ilyasmohaimel/frostgate/internal/process"
	"github.com/ilyasmohaimel/frostgate/internal/profile"
	"github.com/ilyasmohaimel/frostgate/internal/ranking"
	"github.com/ilyasmohaimel/frostgate/internal/recommend"
	"github.com/ilyasmohaimel/frostgate/internal/redact"
	"github.com/ilyasmohaimel/frostgate/internal/report"
	"github.com/ilyasmohaimel/frostgate/internal/route"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
	"github.com/ilyasmohaimel/frostgate/internal/singbox"
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
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	os.Exit(runContext(ctx))
}

func run() int {
	return runContext(context.Background())
}

func runContext(ctx context.Context) int {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "setup":
			return handleSetupContext(ctx, os.Args[2:])
		case "doctor":
			fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			cfgPath := fs.String("config", "", "path to config file")
			verbose := fs.Bool("verbose", false, "verbose output")
			printDoctorUsage := func() { fmt.Fprintln(os.Stdout, "usage: frostgate doctor [--config path] [--verbose]") }
			fs.Usage = func() {}
			if err := fs.Parse(os.Args[2:]); err != nil {
				if err == flag.ErrHelp {
					printDoctorUsage()
					return 0
				}
				fmt.Fprintln(os.Stderr, sanitizeError(err))
				return 3
			}
			if fs.NArg() != 0 {
				fmt.Fprintf(os.Stderr, "unexpected argument %q\n", sanitizeOutput(fs.Arg(0)))
				return 3
			}
			path, managedDir, pathErr := commandPathsChecked(*cfgPath)
			if pathErr != nil {
				fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
				return 3
			}
			return runDoctorContext(ctx, path, managedDir, *verbose)
		case "add":
			return handleAddContext(ctx, os.Args[2:])
		case "import":
			return handleImportContext(ctx, os.Args[2:])
		case "test":
			return handleTestContext(ctx, os.Args[2:])
		case "benchmark":
			return handleBenchmarkContext(ctx, os.Args[2:])
		case "diagnose":
			return handleDiagnoseContext(ctx, os.Args[2:])
		case "network-check", "network_check":
			return handleNetworkCheck(os.Args[2:])
		case "compare":
			return handleCompare(os.Args[2:])
		case "recommend":
			return handleRecommend(os.Args[2:])
		case "history":
			return handleHistory(os.Args[2:])
		case "completion":
			return handleCompletion(os.Args[2:])
		case "help", "--help", "-h":
			printHelp()
			return 0
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n", sanitizeOutput(os.Args[1]))
			return 3
		}
	}

	fs := flag.NewFlagSet("frostgate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "", "path to config file (default: $XDG_CONFIG_HOME/frostgate/config.yaml, ~/.config/frostgate/config.yaml, ./frostgate.yaml)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	watch := fs.Bool("watch", false, "re-probe continuously with TUI (or NDJSON if --json)")
	interval := fs.Duration("interval", 5*time.Second, "interval between probes in --watch")
	timeout := fs.Duration("timeout", 0, "override per-probe timeout (e.g. 3s)")
	doctorFlag := fs.Bool("doctor", false, "run diagnostics and exit")
	checkFlag := fs.Bool("check", false, "validate config and exit")
	versionFlag := fs.Bool("version", false, "print version and exit")
	verbose := fs.Bool("verbose", false, "verbose/debug output")
	debug := fs.Bool("debug", false, "debug output (alias for --verbose)")
	setupFlag := fs.Bool("setup", false, "run interactive setup wizard")
	fs.Usage = func() { printHelp() }
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp()
			return 0
		}
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	if args := fs.Args(); len(args) > 0 {
		fmt.Fprintf(os.Stderr, "unknown command %q\n", sanitizeOutput(args[0]))
		return 3
	}
	if *verbose || *debug {
		*verbose = true
	}
	if *versionFlag {
		fmt.Printf("frostgate %s\n", version)
		return 0
	}
	if *setupFlag {
		if *cfgPath != "" {
			fmt.Fprintln(os.Stderr, "--config is not supported with --setup; setup initializes only the historical default profile")
			return 3
		}
		home := setup.DefaultHomeDir()
		if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
			home = h
		}
		if err := runInteractiveSetupContext(ctx, home); err != nil {
			fmt.Fprintln(os.Stderr, "setup:", sanitizeError(err))
			return 3
		}
		return 0
	}
	if *doctorFlag {
		path, managedDir, pathErr := commandPathsChecked(*cfgPath)
		if pathErr != nil {
			fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
			return 3
		}
		return runDoctorContext(ctx, path, managedDir, *verbose)
	}
	resolved, managedDir, pathErr := commandPathsChecked(*cfgPath)
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
		return 3
	}
	if *checkFlag {
		if _, err := config.Load(resolved); err != nil {
			fmt.Fprintf(os.Stderr, "config check FAIL: %s\n", sanitizeError(err))
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
		printConfigError(err)
		if *verbose {
			fmt.Fprintf(os.Stderr, "config path: %s\n", resolved)
		}
		return 3
	}
	if *timeout < 0 {
		fmt.Fprintln(os.Stderr, "--timeout must be non-negative")
		return 3
	}
	if *timeout > 0 {
		applyTimeoutOverride(cfg, *timeout)
	}
	if *watch {
		if *interval <= 0 {
			fmt.Fprintln(os.Stderr, "--interval must be positive in --watch mode")
			return 3
		}
		profileID := historyProfileForPaths(resolved, managedDir)
		if *jsonOut {
			return watchJSON(ctx, cfg, profileID, *interval, *verbose)
		}
		return watchTUI(ctx, cfg, profileID, *interval, *verbose)
	}
	res := probe.Run(ctx, cfg)
	profileID := historyProfileForPaths(resolved, managedDir)
	rec := newRecorder(cfg, profileID)
	var historyErr error
	if ctx.Err() == nil {
		historyErr = rec.Record(history.FromProbeResults(res, nil, "", profileID))
	}
	if closeErr := rec.Close(); closeErr != nil {
		historyErr = errors.Join(historyErr, closeErr)
	}
	if historyErr != nil {
		fmt.Fprintln(os.Stderr, "history:", sanitizeError(historyErr))
	}
	if err := emit(res, *jsonOut, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "frostgate:", sanitizeError(err))
		return 3
	}
	if historyErr != nil {
		return 3
	}
	return exitCode(res)
}

func runDoctor(path, managedDir string, verbose bool) int {
	return runDoctorContext(context.Background(), path, managedDir, verbose)
}

func runDoctorContext(_ context.Context, path, managedDir string, verbose bool) int {
	if _, err := config.Load(path); err != nil {
		printConfigError(err)
		return 3
	}
	checks := doctor.RunForProfile(path, managedDir, verbose)
	fmt.Print(doctor.FormatColor(checks))
	for _, c := range checks {
		if c.Status == "FAIL" {
			return 1
		}
	}
	return 0
}

func handleSetup(args []string) int {
	return handleSetupContext(context.Background(), args)
}

func handleSetupContext(ctx context.Context, args []string) int {
	if len(args) > 0 {
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
			fmt.Fprintln(os.Stdout, "usage: frostgate setup")
			return 0
		}
		if args[0] == "--config" || strings.HasPrefix(args[0], "--config=") {
			fmt.Fprintln(os.Stderr, "--config is not supported with setup; setup initializes only the historical default profile")
		} else {
			fmt.Fprintf(os.Stderr, "unexpected argument %q\n", sanitizeOutput(args[0]))
		}
		return 3
	}
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	if err := runInteractiveSetupContext(ctx, home); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", sanitizeError(err))
		return 3
	}
	return 0
}

func runInteractiveSetup(home string) error {
	return runInteractiveSetupContext(context.Background(), home)
}

func runInteractiveSetupContext(ctx context.Context, home string) error {
	cfgPath := filepath.Join(setup.ConfigDir(home), "config.yaml")
	lock, err := lockfile.Acquire(ctx, "profile:"+profile.CanonicalPath(cfgPath))
	if err != nil {
		return fmt.Errorf("profile is busy: %w", err)
	}
	defer lock.Close()
	return setup.RunInteractive(setup.Options{
		HomeDir: home,
		Context: ctx,
		AddRoute: func(dir, link, tag string) error {
			result, err := addManagedRouteContext(ctx, dir, filepath.Join(dir, "config.yaml"), strings.TrimSpace(link), tag, nil)
			if err != nil {
				return err
			}
			if result.AlreadyExists {
				fmt.Printf("Route already exists: %s\n", result.Tag)
			}
			return nil
		},
	})
}

func printHelp() {
	fmt.Fprintf(os.Stderr, "frostgate %s\n\nUsage:\n  frostgate [flags]\n", version)
	for _, spec := range commandSpecs {
		fmt.Fprintf(os.Stderr, "  %s\n", spec.Usage)
	}
	fmt.Fprintln(os.Stderr, "\nCommands:")
	for _, spec := range commandSpecs {
		fmt.Fprintf(os.Stderr, "  %-15s %s\n", spec.Name, spec.Summary)
	}
	fmt.Fprintln(os.Stderr, "\nGlobal flags:")
	for _, spec := range globalFlags {
		value := ""
		if spec.TakesValue {
			value = " <value>"
		}
		fmt.Fprintf(os.Stderr, "  --%-13s %s\n", spec.Name+value, spec.Description)
	}
	fmt.Fprintln(os.Stderr, "\nExit codes: 0 all up  1 mixed/degraded  2 all down  3 config/error")
	fmt.Fprintln(os.Stderr, "\nConfig lookup: --config, $XDG_CONFIG_HOME/frostgate/config.yaml, ~/.config/frostgate/config.yaml, ./frostgate.yaml")
}

// --- add ---
func handleAdd(args []string) int {
	return handleAddContext(context.Background(), args)
}

func handleAddContext(ctx context.Context, args []string) int {
	nameFlag := ""
	cfgPath := ""
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--name" && i+1 < len(args) {
			nameFlag = args[i+1]
			i++
		} else if len(a) > 7 && a[:7] == "--name=" {
			nameFlag = a[7:]
		} else if a == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		} else if len(a) > 9 && a[:9] == "--config=" {
			cfgPath = a[9:]
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate add <link> [--name name] [--config path]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", sanitizeOutput(a))
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) != 1 {
		fmt.Fprintln(os.Stderr, "usage: frostgate add <link> [--name name]")
		return 3
	}
	link := strings.TrimSpace(positionals[0])
	if link == "" {
		fmt.Fprintln(os.Stderr, "empty link")
		return 3
	}
	cfgPath2, dir, pathErr := commandPathsChecked(cfgPath)
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
		return 3
	}
	lock, lockErr := lockfile.Acquire(ctx, "profile:"+profile.CanonicalPath(cfgPath2))
	if lockErr != nil {
		fmt.Fprintln(os.Stderr, "profile:", sanitizeError(lockErr))
		return 3
	}
	defer lock.Close()
	rollbackMigration, finalizeMigration, err := prepareManagedStateWithRollback(cfgPath2, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	if err := setup.EnsureConfigDir(dir); err != nil {
		if rollbackErr := rollbackMigration(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	tag := strings.TrimSpace(nameFlag)
	if tag == "" {
		tag = route.SanitizeName(route.DeriveNameFromLink(link, 1))
	}
	fmt.Println("Importing route...")
	result, err := addManagedRouteContext(ctx, dir, cfgPath2, link, tag, nil)
	if err != nil {
		if rollbackErr := rollbackMigration(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	if result.AlreadyExists {
		if err := rollbackMigration(); err != nil {
			fmt.Fprintln(os.Stderr, sanitizeError(err))
			return 3
		}
		fmt.Printf("Route already exists: %s\n", result.Tag)
		return 0
	}
	if err := finalizeMigration(); err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	fmt.Println("sing-box config valid")
	fmt.Println("sidecar reloaded and verified")
	fmt.Println("Testing new route...")
	cfg, err := config.Load(cfgPath2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config load failed:", sanitizeError(err))
		return 3
	}
	var ep *config.Endpoint
	for i, e := range cfg.Endpoints {
		if e.Name == tag {
			ep = &cfg.Endpoints[i]
			break
		}
	}
	if ep != nil {
		probeCtx, cancel := context.WithTimeout(ctx, cfg.ProbeBudget(*ep))
		res := probe.ProbeEndpoint(probeCtx, cfg, *ep)
		cancel()
		if res.Status == "down" {
			fmt.Printf("end-to-end test failed: %s\n", sanitizeError(fmt.Errorf("%s", res.Category)))
			fmt.Println("Route added, but test failed. Run frostgate diagnose.")
			return 1
		}
		jitter := "N/A"
		if res.Metrics.JitterKnown {
			jitter = fmtDuration(res.Metrics.Jitter)
		}
		fmt.Printf("\n%s\nLatency: %s\nLoss: %.0f%%\nJitter: %s\n", tag, resultMedian(res), res.Metrics.LossPct, jitter)
	}
	fmt.Println("\nRoute added.")
	return 0
}

func sanitizeError(err error) string {
	return redact.Error(err)
}

func printConfigError(err error) {
	fmt.Fprintln(os.Stderr, "config:", sanitizeError(err))
}

func sanitizeOutput(s string) string {
	return redact.String(s)
}

// --- import ---
func handleImport(args []string) int {
	return handleImportContext(context.Background(), args)
}

func handleImportContext(ctx context.Context, args []string) int {
	cfgPath := ""
	allowInsecure := false
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--allow-insecure-http" {
			allowInsecure = true
		} else if a == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--config=") {
			cfgPath = strings.TrimPrefix(a, "--config=")
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate import <subscription-url> [--allow-insecure-http] [--config path]")
			return 0
		} else if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", sanitizeOutput(a))
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) != 1 {
		fmt.Fprintln(os.Stderr, "usage: frostgate import <subscription-url> [--allow-insecure-http]")
		return 3
	}
	cfgPath2, dir, pathErr := commandPathsChecked(cfgPath)
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
		return 3
	}
	lock, lockErr := lockfile.Acquire(ctx, "profile:"+profile.CanonicalPath(cfgPath2))
	if lockErr != nil {
		fmt.Fprintln(os.Stderr, "profile:", sanitizeError(lockErr))
		return 3
	}
	defer lock.Close()
	subURL := positionals[0]
	fmt.Fprintln(os.Stderr, "Fetching subscription...")
	data, err := importer.FetchSubscriptionWithOpts(ctx, subURL, allowInsecure)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch failed: %v\n", sanitizeError(err))
		return 3
	}
	res := importer.ParseSubscription(data)
	if len(res.Links) == 0 {
		fmt.Fprintln(os.Stderr, "no usable routes found")
		for _, s := range res.Skipped {
			fmt.Fprintf(os.Stderr, "skip: %s\n", sanitizeOutput(s.Reason))
		}
		return 3
	}
	fmt.Printf("Found %d links, %d skipped\n", len(res.Links), len(res.Skipped))
	rollbackMigration, finalizeMigration, err := prepareManagedStateWithRollback(cfgPath2, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	if err := setup.EnsureConfigDir(dir); err != nil {
		if rollbackErr := rollbackMigration(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	// Deduplicate via fingerprint against existing and within subscription
	mg, err := managedconfig.Load(dir)
	if err != nil {
		if rollbackErr := rollbackMigration(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		fmt.Fprintln(os.Stderr, "load managed config:", sanitizeError(err))
		return 3
	}
	seen := map[string]bool{}
	for _, v := range mg.VLESS {
		seen[route.FingerprintVLESS(v)] = true
	}
	for _, h := range mg.Hysteria {
		seen[route.FingerprintHysteria(h)] = true
	}
	existingTags := map[string]bool{}
	for _, v := range mg.VLESS {
		existingTags[v.Tag] = true
	}
	for _, h := range mg.Hysteria {
		existingTags[h.Tag] = true
	}
	var toAdd []*route.ImportedRoute
	skippedDedup := 0
	skippedInvalid := 0
	for i, link := range res.Links {
		tag := route.SanitizeName(route.DeriveNameFromLink(link, i+1))
		origTag := tag
		suffix := 1
		for existingTags[tag] {
			suffix++
			suffixText := fmt.Sprintf("-%d", suffix)
			base := origTag
			if len(base)+len(suffixText) > route.MaxRouteNameLen {
				base = base[:route.MaxRouteNameLen-len(suffixText)]
			}
			tag = base + suffixText
		}
		r, err := route.ParseShareLink(link, tag)
		if err != nil {
			skippedInvalid++
			continue
		}
		if seen[r.Fingerprint] {
			skippedDedup++
			continue
		}
		seen[r.Fingerprint] = true
		existingTags[tag] = true
		toAdd = append(toAdd, r)
	}
	if len(toAdd) == 0 {
		if err := rollbackMigration(); err != nil {
			fmt.Fprintln(os.Stderr, sanitizeError(err))
			return 3
		}
		fmt.Fprintln(os.Stderr, "all routes were duplicates or invalid")
		return 3
	}
	for _, r := range toAdd {
		if r.VLESS != nil {
			mg.AddVLESS(r.VLESS)
		}
		if r.Hysteria != nil {
			mg.AddHysteria(r.Hysteria)
		}
	}
	tags := make([]string, 0, len(toAdd))
	for _, imported := range toAdd {
		tags = append(tags, imported.Tag)
	}
	if err := commitManagedRoutesContext(ctx, mg, cfgPath2, tags, nil); err != nil {
		if rollbackErr := rollbackMigration(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	if err := finalizeMigration(); err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	fmt.Printf("Imported %d routes (%d skipped, %d deduped)\n", len(toAdd), len(res.Skipped)+skippedInvalid, skippedDedup)
	if len(res.Skipped) > 0 {
		fmt.Println("\nSkipped:")
		for _, s := range res.Skipped {
			fmt.Printf("- %s\n", sanitizeOutput(s.Reason))
		}
	}
	cfg, err := config.Load(cfgPath2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load imported config:", sanitizeError(err))
		return 3
	}
	importedCfg := *cfg
	importedCfg.Endpoints = nil
	importedTags := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		importedTags[tag] = struct{}{}
	}
	for _, endpoint := range cfg.Endpoints {
		if _, ok := importedTags[endpoint.Name]; ok {
			importedCfg.Endpoints = append(importedCfg.Endpoints, endpoint)
		}
	}
	if len(importedCfg.Endpoints) == 0 {
		fmt.Fprintln(os.Stderr, "imported routes were not present in the saved config")
		return 3
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeRunBudget(&importedCfg))
	results := probe.Run(probeCtx, &importedCfg)
	cancel()
	fmt.Println("\nTESTING...")
	for i, r := range results {
		status := string(r.Status)
		fmt.Printf("%2d. %-20s %-8s %4.0f%% %6s\n", i+1, r.Endpoint.Name, status, r.Metrics.LossPct, resultMedian(r))
	}
	usable := 0
	for _, r := range results {
		if r.Status != "down" {
			usable++
		}
	}
	fmt.Printf("\n%d/%d usable.\n", usable, len(results))
	return exitCode(results)
}

// --- test ---

func handleTest(args []string) (exitCode int) {
	return handleTestContext(context.Background(), args)
}

func handleTestContext(ctx context.Context, args []string) (exitCode int) {
	jsonOut := false
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			jsonOut = true
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate test <link> [--json]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", sanitizeOutput(a))
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) != 1 {
		fmt.Fprintln(os.Stderr, "usage: frostgate test <link> [--json]")
		return 3
	}
	link := positionals[0]
	tag := "test-temp"
	r, err := route.ParseShareLink(link, tag)
	if err != nil {
		return testCommandFailure(jsonOut, "", "error", fmt.Errorf("parse: %w", err), 3)
	}
	if jsonOut {
		fmt.Fprintln(os.Stderr, "Testing route (temporary, not saved)...")
	} else {
		fmt.Println("Testing route (temporary, not saved)...")
	}
	tmpDir, err := os.MkdirTemp("", "frostgate-test-*")
	if err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", err, 3)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(tmpDir); cleanupErr != nil {
			fmt.Fprintln(os.Stderr, "remove temporary test state:", sanitizeError(cleanupErr))
			if exitCode == 0 {
				exitCode = 3
			}
		}
	}()
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("secure temporary directory: %w", err), 3)
	}
	secret, err := setup.GenerateSecret()
	if err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("generate controller secret: %w", err), 3)
	}
	var vlessOut []*setup.VLESSOutbound
	var hystOut []*setup.Hysteria2Outbound
	if r.VLESS != nil {
		vlessOut = append(vlessOut, r.VLESS)
	}
	if r.Hysteria != nil {
		hystOut = append(hystOut, r.Hysteria)
	}
	port, err := freePort()
	if err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", err, 3)
	}
	mixedPort, err := freePort()
	if err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", err, 3)
	}
	for mixedPort == port {
		mixedPort, err = freePort()
		if err != nil {
			return testCommandFailure(jsonOut, r.Protocol, "error", err, 3)
		}
	}
	controller := fmt.Sprintf("http://127.0.0.1:%d", port)
	singData, err := setup.GenerateSingBoxConfig(secret, vlessOut, hystOut, tmpDir)
	if err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", err, 3)
	}
	var cfgMap map[string]any
	if err := json.Unmarshal(singData, &cfgMap); err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("parse generated sing-box config: %w", err), 3)
	}
	if exp, ok := cfgMap["experimental"].(map[string]any); ok {
		if clash, ok := exp["clash_api"].(map[string]any); ok {
			clash["external_controller"] = fmt.Sprintf("127.0.0.1:%d", port)
		}
	}
	// Patch mixed inbound port
	if inbounds, ok := cfgMap["inbounds"].([]any); ok && len(inbounds) > 0 {
		if m, ok := inbounds[0].(map[string]any); ok {
			m["listen_port"] = float64(mixedPort)
		}
	}
	singData, err = json.Marshal(cfgMap)
	if err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("encode generated sing-box config: %w", err), 3)
	}
	singPath := filepath.Join(tmpDir, "sing-box.json")
	if err := os.WriteFile(singPath, singData, 0o600); err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("write temporary sing-box config: %w", err), 3)
	}
	if err := os.Chmod(singPath, 0o600); err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("secure temporary sing-box config: %w", err), 3)
	}
	if sbPath, err := exec.LookPath("sing-box"); err == nil {
		cmd, cancel := process.CommandContext(ctx, process.ValidationTimeout, sbPath, "check", "-c", singPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			cancel()
			return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("sing-box check failed: %s", sanitizeOutput(string(out))), 3)
		}
		cancel()
	} else {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("sing-box not found"), 3)
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	sbPath, err := exec.LookPath("sing-box")
	if err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("sing-box not found: %w", err), 3)
	}
	cmd := exec.CommandContext(ctx, sbPath, "run", "-c", singPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", err, 3)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
	}()
	client := singbox.New(controller, secret)
	var ready bool
	for i := 0; i < 25; i++ {
		readyTimer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !readyTimer.Stop() {
				<-readyTimer.C
			}
			return testCommandFailure(jsonOut, r.Protocol, "error", ctx.Err(), 3)
		case <-readyTimer.C:
		}
		cctx, ccancel := context.WithTimeout(ctx, 1*time.Second)
		_, err := client.GetProxies(cctx)
		ccancel()
		if err == nil {
			ready = true
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if !ready {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("controller not ready"), 3)
	}
	timeout := 8 * time.Second
	delayCtx, cancel2 := context.WithTimeout(ctx, timeout)
	delay, err := client.Delay(delayCtx, tag, "https://www.gstatic.com/generate_204", timeout)
	cancel2()
	if err != nil {
		if jsonOut {
			if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol": strings.ToUpper(r.Protocol), "status": "down", "error": sanitizeError(err)}); err != nil {
				fmt.Fprintln(os.Stderr, "encode test JSON:", sanitizeError(err))
				return 3
			}
		} else {
			fmt.Printf("Protocol: %s\nStatus:   DOWN\nError: %s\n\nThis route was NOT saved.\nUse `frostgate add` to keep it.\n", strings.ToUpper(r.Protocol), sanitizeError(err))
		}
		return 1
	}
	mbps := 0.0
	// Try to measure speed via SOCKS if selector exists
	if vlessOut != nil || hystOut != nil {
		sctx, scancel := context.WithTimeout(ctx, 20*time.Second)
		m, _, err := benchmark.MeasureViaSOCKS(sctx, fmt.Sprintf("127.0.0.1:%d", mixedPort), benchmark.DefaultDownloadURL, 5*1024*1024)
		scancel()
		if err == nil {
			mbps = m
		}
	}
	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol": strings.ToUpper(r.Protocol), "status": "up", "median_ms": delay.Milliseconds(), "download_mbps": mbps}); err != nil {
			fmt.Fprintln(os.Stderr, "encode test JSON:", sanitizeError(err))
			return 3
		}
	} else {
		fmt.Printf("Protocol: %s\nStatus:   HEALTHY\nLoss:     0%%\nMedian:   %dms\nJitter:   N/A\n", strings.ToUpper(r.Protocol), delay.Milliseconds())
		if mbps > 0 {
			fmt.Printf("Download: %.1f Mbps\n", mbps)
		}
		fmt.Println("\nThis route was NOT saved.")
		fmt.Println("Use `frostgate add` to keep it.")
	}
	return 0
}

func testCommandFailure(asJSON bool, protocol, status string, err error, code int) int {
	safe := sanitizeError(err)
	if asJSON {
		if encodeErr := json.NewEncoder(os.Stdout).Encode(map[string]any{
			"protocol": strings.ToUpper(protocol),
			"status":   status,
			"error":    safe,
		}); encodeErr != nil {
			fmt.Fprintln(os.Stderr, "encode test JSON:", sanitizeError(encodeErr))
			return 3
		}
	} else {
		fmt.Fprintln(os.Stderr, safe)
	}
	return code
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve temporary loopback port: %w", err)
	}
	addr := l.Addr().(*net.TCPAddr)
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("release temporary loopback port: %w", err)
	}
	return addr.Port, nil
}

// --- benchmark ---
func handleBenchmark(args []string) int {
	return handleBenchmarkContext(context.Background(), args)
}

func handleBenchmarkContext(ctx context.Context, args []string) int {
	networkLabel := ""
	noSpeed := false
	jsonOut := false
	cfgPath := ""
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--network" && i+1 < len(args) {
			networkLabel = args[i+1]
			i++
		} else if len(a) > 10 && a[:10] == "--network=" {
			networkLabel = a[10:]
		} else if a == "--no-speed" {
			noSpeed = true
		} else if a == "--json" {
			jsonOut = true
		} else if a == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		} else if len(a) > 9 && a[:9] == "--config=" {
			cfgPath = a[9:]
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate benchmark [--network label] [--no-speed] [--json] [--config path]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", sanitizeOutput(a))
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", sanitizeOutput(positionals[0]))
		return 3
	}
	cfgFile, managedDir, pathErr := commandPathsChecked(cfgPath)
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
		return 3
	}
	lock, lockErr := lockfile.Acquire(ctx, "profile:"+profile.CanonicalPath(cfgFile))
	if lockErr != nil {
		fmt.Fprintln(os.Stderr, "profile:", sanitizeError(lockErr))
		return 3
	}
	defer lock.Close()
	cfg, err := config.Load(cfgFile)
	if err != nil {
		printConfigError(err)
		return 3
	}
	// A pre-v0.4.9 explicit profile stored its sidecar beside the config. A
	// benchmark needs the managed listener, so it is the one read-oriented
	// command that must complete a proven legacy migration before measuring.
	// Otherwise it could silently use or report the wrong profile's listener.
	if migrated, migrateErr := migrateLegacyProfileForBenchmarkIfNeededContext(ctx, cfg, cfgFile, managedDir); migrateErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(migrateErr))
		return 3
	} else if migrated {
		cfg, err = config.Load(cfgFile)
		if err != nil {
			printConfigError(err)
			return 3
		}
	}
	bytesPerRoute := cfg.Benchmark.DownloadBytes
	if bytesPerRoute == 0 {
		bytesPerRoute = benchmark.DefaultBytes
	}
	downloadURL := cfg.Benchmark.DownloadURL
	if downloadURL == "" {
		downloadURL = benchmark.DefaultDownloadURL
	}
	measurements := benchmarkMeasurementCount(cfg)
	totalBytes := saturatingPositiveProduct(int64(measurements), bytesPerRoute)
	if !noSpeed && measurements > 10 {
		msg := fmt.Sprintf("%d unique benchmark measurements selected. Default benchmark will transfer ~%d MiB.\n", measurements, totalBytes/1024/1024)
		if jsonOut {
			fmt.Fprint(os.Stderr, msg)
			fmt.Fprint(os.Stderr, "Continue? [y/N]: ")
		} else {
			fmt.Print(msg)
			fmt.Print("Continue? [y/N]: ")
		}
		reader := bufio.NewReader(os.Stdin)
		ans, readErr := reader.ReadString('\n')
		if readErr != nil && strings.TrimSpace(ans) == "" {
			fmt.Fprintln(os.Stderr, "benchmark confirmation:", sanitizeError(readErr))
			return 3
		}
		if !strings.EqualFold(strings.TrimSpace(ans), "y") && !strings.EqualFold(strings.TrimSpace(ans), "yes") {
			if jsonOut {
				fmt.Fprintln(os.Stderr, "Cancelled")
				if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "cancelled", "results": []any{}}); err != nil {
					fmt.Fprintln(os.Stderr, "encode benchmark JSON:", sanitizeError(err))
					return 3
				}
			} else {
				fmt.Println("Cancelled")
			}
			return 0
		}
	}
	results := probe.Run(ctx, cfg)
	type benchRes struct {
		Result     probe.Result      `json:"-"`
		JSONResult report.JSONResult `json:"result"`
		Download   float64           `json:"download_mbps"`
		DownloadNA string            `json:"download_na,omitempty"`
	}
	newBenchRes := func(r probe.Result) benchRes {
		return benchRes{Result: r, JSONResult: report.ToJSONResult(r)}
	}
	var bres []benchRes
	downloads := map[string]float64{}
	var benchmarkLifecycleErr error
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	proxyAddr, proxyUnavailableReason := benchmarkProxyAddrWithContext(ctx, cfg, cfgFile, managedDir, nil)
	proxyConfigured := proxyAddr != ""
	if !noSpeed {
		measure := benchmark.MeasureSOCKSMbps
		switch {
		case !proxyConfigured:
			// No loopback listener configured: proxy throughput must be N/A,
			// never guessed. Direct baseline (if alone) still measures direct.
			for _, r := range results {
				br := newBenchRes(r)
				if r.Endpoint.Type == config.ProbeSingBox {
					if r.Status != "down" {
						br.DownloadNA = proxyUnavailableReason
					}
				} else if !probe.IsDirectBaseline(r.Endpoint) {
					br.DownloadNA = "health check endpoint"
				} else {
					sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
					mbps, _, measureErr := benchmark.MeasureDirect(sctx, downloadURL, bytesPerRoute)
					scancel()
					br.Download = mbps
					if measureErr != nil {
						br.DownloadNA = "direct throughput measurement failed"
					}
				}
				downloads[r.Endpoint.Name] = br.Download
				bres = append(bres, br)
			}
		default:
			for _, r := range results {
				if r.Endpoint.Type != config.ProbeSingBox {
					br := newBenchRes(r)
					if !probe.IsDirectBaseline(r.Endpoint) {
						br.DownloadNA = "health check endpoint"
					} else {
						sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
						mbps, _, measureErr := benchmark.MeasureDirect(sctx, downloadURL, bytesPerRoute)
						scancel()
						br.Download = mbps
						if measureErr != nil {
							br.DownloadNA = "direct throughput measurement failed"
						}
					}
					downloads[r.Endpoint.Name] = br.Download
					bres = append(bres, br)
				}
			}
			outboundToNames := map[string][]string{}
			seenOutbound := map[string]bool{}
			var routes []string
			for _, r := range results {
				if r.Endpoint.Type == config.ProbeSingBox && r.Status != "down" {
					outboundToNames[r.Endpoint.Outbound] = append(outboundToNames[r.Endpoint.Outbound], r.Endpoint.Name)
					if !seenOutbound[r.Endpoint.Outbound] {
						seenOutbound[r.Endpoint.Outbound] = true
						routes = append(routes, r.Endpoint.Outbound)
					}
				}
			}
			// Attributed measurement: captures the selector, switches per
			// route, and restores it on success/error/cancellation.
			tp, benchErr := benchmark.BenchRoutesChecked(ctx, client, "frostgate-selector", proxyAddr, downloadURL, bytesPerRoute, routes, measure)
			if benchErr != nil {
				benchmarkLifecycleErr = benchErr
				invalidateBenchmarkThroughput(tp, benchErr)
				fmt.Fprintf(os.Stderr, "benchmark: %s\n", sanitizeError(benchErr))
			}
			tpByName := map[string]benchmark.RouteThroughput{}
			for _, t := range tp {
				for _, name := range outboundToNames[t.Route] {
					tpByName[name] = t
				}
			}
			for _, r := range results {
				if r.Endpoint.Type != config.ProbeSingBox {
					continue
				}
				br := newBenchRes(r)
				if t, ok := tpByName[r.Endpoint.Name]; ok {
					br.Download = t.DownloadMbps
					br.DownloadNA = t.NAReason
				} else if benchmarkLifecycleErr != nil && r.Status != "down" {
					br.DownloadNA = "selector state unavailable"
				}
				downloads[r.Endpoint.Name] = br.Download
				bres = append(bres, br)
			}
		}
	} else {
		for _, r := range results {
			bres = append(bres, newBenchRes(r))
		}
	}
	// History via the central recorder (honors enabled/retention/watch_interval).
	profileID := historyProfileForPaths(cfgFile, managedDir)
	recCfg := newRecorder(cfg, profileID)
	var historyErr error
	if recCfg.Enabled() && ctx.Err() == nil {
		historyErr = recCfg.Record(history.FromProbeResults(results, func(name string) float64 { return downloads[name] }, networkLabel, profileID))
	}
	if closeErr := recCfg.Close(); closeErr != nil {
		historyErr = errors.Join(historyErr, closeErr)
	}
	if historyErr != nil {
		fmt.Fprintln(os.Stderr, "history:", sanitizeError(historyErr))
	}
	if jsonOut {
		networkOutput := networkLabel
		if networkOutput != "" {
			networkOutput = redact.String(networkOutput)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"schema_version": report.JSONSchemaVersion, "results": bres, "network": networkOutput}); err != nil {
			fmt.Fprintln(os.Stderr, "encode benchmark JSON:", sanitizeError(err))
			return 3
		}
		if historyErr != nil || benchmarkLifecycleErr != nil {
			return 3
		}
		return exitCode(results)
	}
	fmt.Println("\nFROSTGATE BENCHMARK")
	if d, ok := probe.DirectBaseline(results); ok {
		fmt.Printf("Direct baseline: %s %s\n", strings.ToUpper(string(d.Status)), resultMedian(d))
	}
	fmt.Printf("%-20s %-8s %4s %7s %7s %10s\n", "ROUTE", "STATUS", "LOSS", "MEDIAN", "JITTER", "DOWNLOAD")
	for _, br := range bres {
		status := string(br.Result.Status)
		loss := fmt.Sprintf("%.0f%%", br.Result.Metrics.LossPct)
		median := resultMedian(br.Result)
		jitter := "N/A"
		if br.Result.Metrics.JitterKnown {
			jitter = fmtDuration(br.Result.Metrics.Jitter)
		}
		down := fmt.Sprintf("%.0f Mbps", br.Download)
		if br.Download <= 0 {
			down = "N/A"
			if br.DownloadNA != "" {
				down = "N/A (" + br.DownloadNA + ")"
			}
		}
		fmt.Printf("%-20s %-8s %4s %7s %7s %10s\n", br.Result.Endpoint.Name, status, loss, median, jitter, down)
	}
	rec, recommendErr := recommendFromResultsWithDownloads(cfg, results, func(name string) float64 { return downloads[name] }, cfgFile, managedDir)
	if recommendErr != nil {
		fmt.Fprintln(os.Stderr, "history:", sanitizeError(recommendErr))
	}
	if rec.BestOverall.Name != "" {
		fmt.Printf("\nBest proxy: %s\n", rec.BestOverall.Name)
		if why := compactReasons(rec.Reasons); why != "" {
			fmt.Printf("Why: %s\n", why)
		}
		if rec.Preferred != nil {
			fmt.Printf("Preferred: %s\n", rec.Preferred.Name)
		}
	} else if probe.HasProxy(results) {
		fmt.Println("\nBest proxy: none usable")
	}
	if historyErr != nil || recommendErr != nil || benchmarkLifecycleErr != nil {
		return 3
	}
	return exitCode(results)
}

// invalidateBenchmarkThroughput prevents a selector lifecycle failure from
// being shown as an attributed throughput result.
func invalidateBenchmarkThroughput(results []benchmark.RouteThroughput, err error) {
	reason := "attributed benchmark failed"
	if strings.Contains(err.Error(), "restore selector") {
		reason = "selector restoration failed"
	} else if strings.Contains(err.Error(), "capture selector") {
		reason = "selector state unavailable"
	} else if strings.Contains(err.Error(), "switch selector") {
		reason = "selector switch failed"
	}
	for i := range results {
		results[i].DownloadMbps = 0
		results[i].NAReason = reason
	}
}

func compactReasons(reasons []string) string {
	return strings.Join(reasons, " - ")
}

func benchmarkProxyAddr(cfg *config.Config, configPath, managedDir string) (string, string) {
	return benchmarkProxyAddrWithContext(context.Background(), cfg, configPath, managedDir, nil)
}

// benchmarkMeasurementCount is the transfer count used for the confirmation
// estimate. Endpoint aliases sharing one sing-box outbound are one network
// measurement; each non-proxy endpoint still needs its own direct transfer.
func benchmarkMeasurementCount(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	uniqueOutbounds := make(map[string]struct{})
	direct := 0
	for _, endpoint := range cfg.Endpoints {
		if endpoint.Type == config.ProbeSingBox && endpoint.Outbound != "" {
			uniqueOutbounds[endpoint.Outbound] = struct{}{}
			continue
		}
		if endpoint.Type != config.ProbeSingBox && probe.IsDirectBaseline(endpoint) {
			direct++
		}
	}
	return len(uniqueOutbounds) + direct
}

// migrateLegacyProfileForBenchmark makes an old parent-directory sidecar
// usable only when prepareManagedState has proved its ownership. It is kept
// separate from read-only diagnostics: benchmark changes selector/listener
// state by definition, whereas doctor and diagnose must never start or move a
// user service as a side effect.
func migrateLegacyProfileForBenchmark(configPath, managedDir string) (bool, error) {
	return migrateLegacyProfileForBenchmarkContext(context.Background(), configPath, managedDir)
}

func migrateLegacyProfileForBenchmarkContext(ctx context.Context, configPath, managedDir string) (bool, error) {
	if managedDir != profile.StateDir(configPath) {
		return false, nil
	}
	if _, err := os.Lstat(managedDir); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect profile state: %w", err)
	}
	rollback, finalize, err := prepareManagedStateWithRollback(configPath, managedDir)
	if err != nil {
		return false, err
	}
	// No state was copied, so this is a direct-only or brand-new profile.
	if _, err := os.Lstat(managedDir); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect migrated profile state: %w", err)
	}
	mg, err := managedconfig.Load(managedDir)
	if err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		return false, fmt.Errorf("load migrated managed config: %w", err)
	}
	if err := commitManagedRoutesContext(ctx, mg, configPath, nil, nil); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		return false, fmt.Errorf("activate migrated profile: %w", err)
	}
	if err := finalize(); err != nil {
		return false, err
	}
	return true, nil
}

func migrateLegacyProfileForBenchmarkIfNeeded(cfg *config.Config, configPath, managedDir string) (bool, error) {
	return migrateLegacyProfileForBenchmarkIfNeededContext(context.Background(), cfg, configPath, managedDir)
}

func migrateLegacyProfileForBenchmarkIfNeededContext(ctx context.Context, cfg *config.Config, configPath, managedDir string) (bool, error) {
	if !hasProxyEndpoints(cfg) {
		return false, nil
	}
	return migrateLegacyProfileForBenchmarkContext(ctx, configPath, managedDir)
}

func benchmarkProxyAddrWithRunner(cfg *config.Config, configPath, managedDir string, runner *managedconfig.SidecarRunner) (string, string) {
	return benchmarkProxyAddrWithContext(context.Background(), cfg, configPath, managedDir, runner)
}

func benchmarkProxyAddrWithContext(ctx context.Context, cfg *config.Config, configPath, managedDir string, runner *managedconfig.SidecarRunner) (string, string) {
	dir := managedDir
	if dir == "" {
		dir = filepath.Dir(configPath)
	}
	if addr := cfg.BenchmarkProxyAddr(); addr != "" {
		return addr, ""
	}
	mg, err := managedconfig.Load(dir)
	if err != nil {
		return "", "benchmark_proxy not configured"
	}
	seen := map[string]bool{}
	var outbounds []string
	for _, ep := range cfg.Endpoints {
		if ep.Type == config.ProbeSingBox && ep.Outbound != "" && !seen[ep.Outbound] {
			seen[ep.Outbound] = true
			outbounds = append(outbounds, ep.Outbound)
		}
	}
	if mg.HasManagedSidecar() {
		if addr, ok := managedconfig.DiscoverBenchmarkProxy(dir); ok {
			return addr, ""
		}
	}
	if !mg.IsLegacyManagedBenchmarkConfig(cfg.SingBox.Controller, outbounds) {
		return "", "benchmark_proxy not configured"
	}
	// Legacy migration shares the normal managed-route transaction so an
	// explicit profile gets matching config bindings and service lifecycle.
	mg.AdoptLegacyRoutes()
	if err := commitManagedRoutesContext(ctx, mg, configPath, nil, runner); err != nil {
		return "", "managed benchmark migration failed"
	}
	if addr, ok := managedconfig.DiscoverBenchmarkProxy(dir); ok {
		return addr, ""
	}
	return "", "managed benchmark migration failed"
}

func fmtDuration(d time.Duration) string {
	if d == 0 {
		return "0ms"
	}
	if d < time.Millisecond {
		return "<1ms"
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func resultMedian(r probe.Result) string {
	if r.Metrics.Succeeded == 0 {
		return "N/A"
	}
	return fmtDuration(r.Metrics.Median)
}

func saturatingPositiveProduct(left, right int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	if left > math.MaxInt64/right {
		return math.MaxInt64
	}
	return left * right
}

// --- diagnose ---
func handleDiagnose(args []string) int {
	return handleDiagnoseContext(context.Background(), args)
}

func handleDiagnoseContext(ctx context.Context, args []string) int {
	if ctx == nil {
		ctx = context.Background()
	}
	cfgPath := ""
	jsonOut := false
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			jsonOut = true
		} else if a == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--config=") {
			cfgPath = strings.TrimPrefix(a, "--config=")
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate diagnose [route] [--json] [--config path]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", sanitizeOutput(a))
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) > 1 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", sanitizeOutput(positionals[1]))
		return 3
	}
	rest := positionals
	cfgFile, dir, pathErr := commandPathsChecked(cfgPath)
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
		return 3
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		printConfigError(err)
		return 3
	}
	var layers []diagnose.Layer
	opts := diagnose.Options{}
	opts.SidecarService = profileForPaths(cfgFile, dir).Service
	var managedStateErr error
	if mg, err := loadManagedStateForDiagnose(dir); err != nil {
		managedStateErr = err
	} else {
		opts.Managed = mg
	}
	if len(rest) == 0 {
		layers = diagnose.DiagnoseAll(cfg, opts)
	} else {
		layers = diagnose.DiagnoseRoute(cfg, rest[0], opts)
	}
	if managedStateErr != nil {
		layers = append([]diagnose.Layer{{Name: "Managed state", Status: "FAIL", Detail: sanitizeError(managedStateErr), Hint: "Restore or recreate Frostgate-managed sidecar state."}}, layers...)
	}
	hasFail := false
	for _, l := range layers {
		if l.Status == "FAIL" {
			hasFail = true
			break
		}
	}
	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(layers); err != nil {
			fmt.Fprintln(os.Stderr, "encode diagnose JSON:", sanitizeError(err))
			return 3
		}
		if hasFail {
			return 1
		}
		return 0
	}
	// Print
	if len(rest) > 0 {
		fmt.Printf("\n%s\n\n", strings.ToUpper(rest[0]))
	}
	for _, l := range layers {
		status := l.Status
		switch status {
		case "PASS":
			status = "PASS"
		case "FAIL":
			status = "FAIL"
		default:
			status = l.Status
		}
		fmt.Printf("%-25s %-6s %s\n", l.Name, status, l.Detail)
		if l.Hint != "" {
			fmt.Printf("  → %s\n", l.Hint)
		}
	}
	// Likely problem
	if hasFail {
		fmt.Println("\nLikely problem:")
		for _, l := range layers {
			if l.Status == "FAIL" && l.Hint != "" {
				fmt.Printf("- %s: %s\n", l.Name, l.Hint)
			}
		}
		// Backup suggestion
		for _, l := range layers {
			if l.Name == "Backup" && l.Status == "PASS" {
				fmt.Printf("\nBackup:\n%s\n", l.Detail)
			}
		}
	}
	if hasFail {
		return 1
	}
	return 0
}

func loadManagedStateForDiagnose(dir string) (*managedconfig.Managed, error) {
	statePath := filepath.Join(dir, "sing-box.json")
	info, err := os.Lstat(statePath)
	if os.IsNotExist(err) {
		// An external-controller profile may legitimately keep a local
		// controller-secret file. The managed sidecar JSON is the ownership
		// marker, so a secret alone must not become a false diagnosis failure.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect managed state: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("managed state is not a regular file")
	}
	return managedconfig.Load(dir)
}

// --- network-check ---
type networkCheckDoc struct {
	SchemaVersion    int                 `json:"schema_version"`
	Checks           []network.Check     `json:"checks"`
	Matrix           []network.MatrixRow `json:"matrix,omitempty"`
	Destinations     []string            `json:"destinations,omitempty"`
	RecommendedProxy string              `json:"recommended_proxy,omitempty"`
	Recommendation   string              `json:"recommendation"`
}

func handleNetworkCheck(args []string) int {
	cfgPath := ""
	jsonOut := false
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			jsonOut = true
		} else if a == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--config=") {
			cfgPath = strings.TrimPrefix(a, "--config=")
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate network-check [--json] [--config path]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", sanitizeOutput(a))
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", sanitizeOutput(positionals[0]))
		return 3
	}

	doc := networkCheckDoc{SchemaVersion: report.JSONSchemaVersion}
	if os.Getenv("FROSTGATE_TEST_SKIP_BASE_NETWORK_CHECKS") != "1" {
		doc.Checks = network.CheckAll()
	}

	// Config-first destinations; defaults only when the list is empty.
	var destURLs []string
	destLabels := map[string]string{}
	var cfg *config.Config
	resolvedConfig, managedDir, pathErr := commandPathsChecked(cfgPath)
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
		return 3
	}
	if loaded, err := config.Load(resolvedConfig); err == nil {
		cfg = loaded
	} else if cfgPath != "" || !os.IsNotExist(err) {
		// An absent implicit config is valid for generic network checks; an
		// existing but malformed/unreadable implicit config is an error, not a
		// reason to pretend Frostgate has no configuration.
		printConfigError(err)
		return 3
	}
	if cfg != nil && len(cfg.NetworkCheck.Destinations) > 0 {
		for _, d := range cfg.NetworkCheck.Destinations {
			if d.URL == "" {
				continue
			}
			label := d.Name
			if label == "" {
				label = d.URL
			}
			destLabels[d.URL] = label
			destURLs = append(destURLs, d.URL)
		}
	}
	if len(destURLs) == 0 {
		destURLs = append(destURLs, network.DefaultDestinations...)
	}
	for _, dest := range destURLs {
		if label := destLabels[dest]; label != "" {
			doc.Destinations = append(doc.Destinations, label)
		} else {
			doc.Destinations = append(doc.Destinations, redact.String(dest))
		}
	}
	doc.Checks = append(doc.Checks, network.CheckDestinations(destURLs)...)

	// Proxy destination matrix via per-outbound URL-test. Never mutates the
	// benchmark selector.
	if cfg != nil && hasProxyEndpoints(cfg) {
		mg, err := loadManagedConfigForNetworkCheck(cfg, managedDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load managed config:", sanitizeError(err))
			return 3
		}
		routes := proxyRoutesForMatrix(cfg, mg)
		client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
		routeTimeout := networkMatrixRouteTimeout(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), network.MatrixBudget(len(destURLs), len(routes), routeTimeout))
		doc.Matrix = network.DestinationMatrix(ctx, destURLs, routes, client, routeTimeout)
		cancel()
		for i := range doc.Matrix {
			if label := destLabels[doc.Matrix[i].Destination]; label != "" {
				doc.Matrix[i].Destination = label
			} else {
				doc.Matrix[i].Destination = redact.String(doc.Matrix[i].Destination)
			}
		}
	}

	// Lightweight recommendation from a full probe + shared ranking engine.
	recommended := ""
	if cfg != nil {
		results := probeRunQuick(cfg)
		rec, recommendErr := recommendFromResults(cfg, results, resolvedConfig, managedDir)
		if recommendErr != nil {
			fmt.Fprintln(os.Stderr, "history:", sanitizeError(recommendErr))
			return 3
		}
		if rec.BestOverall.Name != "" {
			recommended = rec.BestOverall.Name
		}
	}
	doc.RecommendedProxy = recommended
	if recommended == "" {
		doc.Recommendation = "insufficient data"
	} else {
		doc.Recommendation = recommended
	}

	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(doc); err != nil {
			fmt.Fprintln(os.Stderr, "encode network-check JSON:", sanitizeError(err))
			return 3
		}
		return networkCheckExitCode(doc)
	}

	fmt.Println("\nFROSTGATE NETWORK CHECK")
	fmt.Println("\nConnection")
	printChecks(doc.Checks, func(c network.Check) bool {
		switch c.Name {
		case "IPv4", "IPv6", "HTTPS", "Captive portal":
			return true
		}
		return false
	})
	fmt.Println("\nDNS")
	printChecks(doc.Checks, func(c network.Check) bool {
		return strings.Contains(c.Name, "resolver") || strings.Contains(c.Name, "DNS")
	})
	fmt.Println("\nDestinations")
	printChecks(doc.Checks, func(c network.Check) bool {
		return strings.HasPrefix(c.Name, "http://") || strings.HasPrefix(c.Name, "https://")
	})
	fmt.Println("\nMatrix")
	for _, row := range doc.Matrix {
		fmt.Printf("%s\n", matrixLabel(row.Destination, destLabels))
		dirStatus := strings.Fields(row.Direct)
		directCol := "FAIL"
		if len(dirStatus) > 0 {
			directCol = dirStatus[0]
		}
		fmt.Printf("  %-14s %s\n", "Direct", directCol)
		for _, cell := range row.Routes {
			status := cell.Status
			if status == "PASS" {
				status = "PASS"
			}
			fmt.Printf("  %-14s %-5s (%s)\n", truncateRoute(cell.Route), status, cell.Kind)
		}
	}
	fmt.Println("\nSignals")
	hasDirectFail := false
	hasProxyPass := false
	for _, c := range doc.Checks {
		if strings.HasPrefix(c.Name, "http") && c.Status == "FAIL" && !hasDirectFail {
			hasDirectFail = true
		}
	}
	for _, row := range doc.Matrix {
		for _, cell := range row.Routes {
			if cell.Status == "PASS" {
				hasProxyPass = true
			}
		}
	}
	if hasDirectFail && hasProxyPass {
		fmt.Println("- direct access differs by destination; proxy routes are usable")
	}
	for _, c := range doc.Checks {
		if c.Name == "DNS vs DoH" && c.Status == "WARN" {
			fmt.Println("- DNS answers differ from DoH (signal only)")
		}
	}
	fmt.Println("\nRecommendation")
	if recommended != "" {
		fmt.Printf("Recommended proxy: %s\n", recommended)
	} else {
		fmt.Println("Recommendation: insufficient data")
	}
	return networkCheckExitCode(doc)
}

func applyTimeoutOverride(cfg *config.Config, timeout time.Duration) {
	if cfg == nil || timeout <= 0 {
		return
	}
	cfg.Defaults.Timeout.Duration = timeout
	for i := range cfg.Endpoints {
		cfg.Endpoints[i].Timeout.Duration = timeout
	}
}

func hasProxyEndpoints(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	for _, ep := range cfg.Endpoints {
		if ep.Type == config.ProbeSingBox {
			return true
		}
	}
	return false
}

func loadManagedConfigForNetworkCheck(cfg *config.Config, managedDir string) (*managedconfig.Managed, error) {
	if !hasProxyEndpoints(cfg) {
		return nil, nil
	}
	return managedconfig.Load(managedDir)
}

func networkCheckExitCode(doc networkCheckDoc) int {
	for _, check := range doc.Checks {
		if check.Status == "FAIL" {
			return 1
		}
	}
	for _, row := range doc.Matrix {
		if strings.HasPrefix(row.Direct, "FAIL") {
			return 1
		}
		for _, cell := range row.Routes {
			if cell.Status == "FAIL" {
				return 1
			}
		}
	}
	return 0
}

func printChecks(checks []network.Check, keep func(network.Check) bool) {
	for _, c := range checks {
		if keep(c) {
			fmt.Printf("%-25s %-12s %s\n", c.Name, c.Status, c.Detail)
		}
	}
}

func matrixLabel(url string, labels map[string]string) string {
	if l, ok := labels[url]; ok && l != "" {
		return l
	}
	return url
}

func truncateRoute(name string) string {
	runes := []rune(name)
	if len(runes) > 14 {
		return string(runes[:13]) + "~"
	}
	return name
}

// proxyRoutesForMatrix maps singbox endpoints to matrix columns using managed
// metadata for protocol kinds (reality / hysteria2 / proxy).
func proxyRoutesForMatrix(cfg *config.Config, mg *managedconfig.Managed) []network.RouteProbe {
	var out []network.RouteProbe
	isVLESS := map[string]bool{}
	isHyst := map[string]bool{}
	if mg != nil {
		for _, v := range mg.VLESS {
			if v != nil {
				isVLESS[v.Tag] = true
			}
		}
		for _, h := range mg.Hysteria {
			if h != nil {
				isHyst[h.Tag] = true
			}
		}
	}
	for _, ep := range cfg.Endpoints {
		if ep.Type != config.ProbeSingBox {
			continue
		}
		kind := "proxy"
		switch {
		case isVLESS[ep.Outbound]:
			kind = "reality"
		case isHyst[ep.Outbound]:
			kind = "hysteria2"
		}
		out = append(out, network.RouteProbe{Name: ep.Name, Outbound: ep.Outbound, Kind: kind})
	}
	return out
}

// --- compare ---
type compareRoute struct {
	Route        string  `json:"route"`
	Type         string  `json:"type,omitempty"`
	Availability float64 `json:"availability_pct"`
	Samples      int     `json:"samples"`
	MedianMs     int64   `json:"median_ms"`
	JitterMs     *int64  `json:"jitter_ms"`
	JitterKnown  bool    `json:"jitter_known"`
	DownloadMbps float64 `json:"download_mbps,omitempty"`
	Trend        string  `json:"trend"`
}

type compareNetwork struct {
	Network             string         `json:"network"`
	Routes              []compareRoute `json:"routes"`
	BestProxy           string         `json:"best_proxy,omitempty"`
	BestProxyConfidence string         `json:"best_proxy_confidence,omitempty"`
}

// compareBestProxy ranks a network's routes with the shared ranking engine;
// direct/non-proxy rows are never eligible for Best Proxy.
func compareBestProxy(summaries []history.Summary) (string, string) {
	var cands []ranking.Candidate
	for _, s := range summaries {
		if s.Type != string(config.ProbeSingBox) {
			continue
		}
		status := "up"
		if s.Availability == 0 {
			status = "down"
		} else if s.Availability < 95 {
			status = "degraded"
		}
		cands = append(cands, ranking.Candidate{
			Name:            s.Route,
			IsProxy:         s.Type == string(config.ProbeSingBox),
			Status:          status,
			LossPct:         100 - s.Availability,
			Median:          s.Median,
			Jitter:          s.Jitter,
			JitterKnown:     s.JitterKnown,
			DownloadMbps:    s.Download,
			HistoryCount:    s.Count,
			AvailabilityPct: s.Availability,
		})
	}
	if len(cands) == 0 {
		return "", ""
	}
	best := ranking.Best(cands)
	if best == nil {
		return "", ""
	}
	conf := "low"
	switch {
	case best.HistoryCount >= 50:
		conf = "high"
	case best.HistoryCount >= 10:
		conf = "medium"
	}
	return best.Name, conf
}

func handleCompare(args []string) int {
	jsonOut := false
	cfgPath := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--config" && i+1 < len(args):
			cfgPath = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stderr, "usage: frostgate compare [--json] [--config path]")
			return 0
		case len(a) > 0 && a[0] == '-':
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", sanitizeOutput(a))
			return 3
		default:
			fmt.Fprintf(os.Stderr, "unexpected argument %q\n", sanitizeOutput(a))
			return 3
		}
	}
	configFile, managedDir, pathErr := commandPathsChecked(cfgPath)
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
		return 3
	}
	profileID := historyProfileForPaths(configFile, managedDir)
	db, err := history.OpenReadonly()
	if err != nil {
		if !errors.Is(err, history.ErrNoHistory) {
			fmt.Fprintln(os.Stderr, "history:", sanitizeError(err))
			return 3
		}
		msg := "No network history found. Run:\n  frostgate benchmark --network home\n"
		if jsonOut {
			fmt.Fprintln(os.Stderr, msg)
			if encodeErr := json.NewEncoder(os.Stdout).Encode([]compareNetwork{}); encodeErr != nil {
				fmt.Fprintln(os.Stderr, "encode compare JSON:", sanitizeError(encodeErr))
				return 3
			}
			return 0
		}
		fmt.Print(msg)
		return 0
	}
	records, queryErr := db.QueryProfile(30*24*time.Hour, "", "", profileID, profileID == history.DefaultProfile)
	if queryErr != nil {
		if closeErr := db.Close(); closeErr != nil {
			queryErr = errors.Join(queryErr, fmt.Errorf("close history: %w", closeErr))
		}
		fmt.Fprintln(os.Stderr, "history:", sanitizeError(queryErr))
		return 3
	}
	netSet := map[string]bool{}
	for _, r := range records {
		if r.Network != "" {
			netSet[r.Network] = true
		}
	}
	if len(netSet) == 0 {
		msg := "No network history found. Run:\n  frostgate benchmark --network home\n"
		if jsonOut {
			fmt.Fprintln(os.Stderr, msg)
			if encodeErr := json.NewEncoder(os.Stdout).Encode([]compareNetwork{}); encodeErr != nil {
				if closeErr := db.Close(); closeErr != nil {
					encodeErr = errors.Join(encodeErr, fmt.Errorf("close history: %w", closeErr))
				}
				fmt.Fprintln(os.Stderr, "encode compare JSON:", sanitizeError(encodeErr))
				return 3
			}
			if closeErr := db.Close(); closeErr != nil {
				fmt.Fprintln(os.Stderr, "history:", sanitizeError(closeErr))
				return 3
			}
			return 0
		}
		if closeErr := db.Close(); closeErr != nil {
			fmt.Fprintln(os.Stderr, "history:", sanitizeError(closeErr))
			return 3
		}
		fmt.Print(msg)
		return 0
	}
	networks := make([]string, 0, len(netSet))
	for n := range netSet {
		networks = append(networks, n)
	}
	sort.Strings(networks)

	out := make([]compareNetwork, 0, len(networks))
	for _, netw := range networks {
		recs, readErr := db.QueryProfile(30*24*time.Hour, "", netw, profileID, profileID == history.DefaultProfile)
		if readErr != nil {
			if closeErr := db.Close(); closeErr != nil {
				readErr = errors.Join(readErr, fmt.Errorf("close history: %w", closeErr))
			}
			fmt.Fprintln(os.Stderr, "history:", sanitizeError(readErr))
			return 3
		}
		summ := history.Summarize(recs)
		sort.Slice(summ, func(i, j int) bool { return summ[i].Route < summ[j].Route })
		cn := compareNetwork{Network: netw, Routes: make([]compareRoute, 0, len(summ))}
		for _, s := range summ {
			var jitterMs *int64
			if s.JitterKnown {
				value := s.Jitter.Milliseconds()
				jitterMs = &value
			}
			cn.Routes = append(cn.Routes, compareRoute{
				Route:        s.Route,
				Type:         s.Type,
				Availability: s.Availability,
				Samples:      s.Count,
				MedianMs:     s.Median.Milliseconds(),
				JitterMs:     jitterMs,
				JitterKnown:  s.JitterKnown,
				DownloadMbps: s.Download,
				Trend:        s.Trend,
			})
		}
		cn.BestProxy, cn.BestProxyConfidence = compareBestProxy(summ)
		out = append(out, cn)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			if closeErr := db.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close history: %w", closeErr))
			}
			fmt.Fprintln(os.Stderr, "encode compare JSON:", sanitizeError(err))
			return 3
		}
		if err := db.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "history:", sanitizeError(err))
			return 3
		}
		return 0
	}
	fmt.Println("\nNETWORK COMPARISON")
	fmt.Println()
	header := fmt.Sprintf("%-20s", "")
	for _, cn := range out {
		header += fmt.Sprintf("%-14s", strings.ToUpper(cn.Network))
	}
	fmt.Println(header)
	routeNames := map[string]bool{}
	for _, cn := range out {
		for _, r := range cn.Routes {
			routeNames[r.Route] = true
		}
	}
	sortedRoutes := make([]string, 0, len(routeNames))
	for rn := range routeNames {
		sortedRoutes = append(sortedRoutes, rn)
	}
	sort.Strings(sortedRoutes)

	fmt.Println()
	for _, route := range sortedRoutes {
		fmt.Printf("%s\n", route)
		for _, cn := range out {
			var cr *compareRoute
			for i := range cn.Routes {
				if cn.Routes[i].Route == route {
					cr = &cn.Routes[i]
				}
			}
			if cr == nil {
				continue
			}
			dl := ""
			if cr.DownloadMbps > 0 {
				dl = fmt.Sprintf(" download %.0fMb", cr.DownloadMbps)
			}
			jitter := "N/A"
			if cr.JitterMs != nil {
				jitter = fmt.Sprintf("%dms", *cr.JitterMs)
			}
			fmt.Printf("  %-12s avail %3.0f%%  samples %-4d median %4dms  jitter %s%s\n",
				cn.Network, cr.Availability, cr.Samples, cr.MedianMs, jitter, dl)
		}
	}
	fmt.Println("\nBest proxy per network:")
	for _, cn := range out {
		if cn.BestProxy == "" {
			fmt.Printf("%-12s (none usable)\n", cn.Network)
			continue
		}
		fmt.Printf("%-12s %s (confidence: %s)\n", cn.Network, cn.BestProxy, cn.BestProxyConfidence)
	}
	if err := db.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "history:", sanitizeError(err))
		return 3
	}
	return 0
}

// --- recommend ---
func handleRecommend(args []string) int {
	jsonOut := false
	cfgPath := ""
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			jsonOut = true
		} else if a == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--config=") {
			cfgPath = strings.TrimPrefix(a, "--config=")
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate recommend [--json] [--config path]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", sanitizeOutput(a))
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", sanitizeOutput(positionals[0]))
		return 3
	}
	cfgFile, managedDir, pathErr := commandPathsChecked(cfgPath)
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
		return 3
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		printConfigError(err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeRunBudget(cfg))
	results := probe.Run(ctx, cfg)
	cancel()
	rec, recommendErr := recommendFromResults(cfg, results, cfgFile, managedDir)
	if recommendErr != nil {
		fmt.Fprintln(os.Stderr, "history:", sanitizeError(recommendErr))
		return 3
	}
	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(rec); err != nil {
			fmt.Fprintln(os.Stderr, "encode recommend JSON:", sanitizeError(err))
			return 3
		}
		return 0
	}
	fmt.Println("\nRECOMMENDATION")
	if d, ok := probe.DirectBaseline(results); ok {
		fmt.Printf("Direct baseline: %s %s\n", strings.ToUpper(string(d.Status)), resultMedian(d))
	}
	if rec.BestOverall.Name == "" {
		fmt.Println("Best proxy: none usable")
		fmt.Println("Confidence: insufficient data")
		return 0
	}
	fmt.Printf("Best proxy: %s\n", rec.BestOverall.Name)
	if why := compactReasons(rec.Reasons); why != "" {
		fmt.Printf("Why: %s\n", why)
	}
	if rec.Preferred != nil {
		fmt.Printf("Preferred: %s\n", rec.Preferred.Name)
	}
	if rec.Fastest.Name != "" && rec.Fastest.Name != rec.BestOverall.Name {
		fmt.Printf("Fastest: %s\n", rec.Fastest.Name)
	}
	if rec.MostStable.Name != "" && rec.MostStable.Name != rec.BestOverall.Name {
		fmt.Printf("Most stable: %s\n", rec.MostStable.Name)
	}
	if len(rec.Avoid) > 0 {
		fmt.Println("Avoid:")
		for _, a := range rec.Avoid {
			fmt.Printf("- %s (%.0f%% loss)\n", a.Name, a.Loss)
		}
	}
	fmt.Printf("Confidence: %s (%s)\n", rec.Confidence, rec.ConfidenceDetail)
	return 0
}

// --- history ---
func handleHistory(args []string) int {
	sinceStr := "24h"
	route := ""
	networkLabel := ""
	jsonOut := false
	cfgPath := ""
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			jsonOut = true
		} else if a == "--since" && i+1 < len(args) {
			sinceStr = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--since=") {
			sinceStr = strings.TrimPrefix(a, "--since=")
		} else if a == "--route" && i+1 < len(args) {
			route = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--route=") {
			route = strings.TrimPrefix(a, "--route=")
		} else if a == "--network" && i+1 < len(args) {
			networkLabel = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--network=") {
			networkLabel = strings.TrimPrefix(a, "--network=")
		} else if a == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
		} else if strings.HasPrefix(a, "--config=") {
			cfgPath = strings.TrimPrefix(a, "--config=")
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate history [--since duration] [--route name] [--network label] [--config path] [--json]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", sanitizeOutput(a))
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", sanitizeOutput(positionals[0]))
		return 3
	}
	since, err := parseSince(sinceStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --since: %v\n", err)
		return 3
	}
	configFile, managedDir, pathErr := commandPathsChecked(cfgPath)
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(pathErr))
		return 3
	}
	db, err := history.OpenReadonly()
	if err != nil {
		if errors.Is(err, history.ErrNoHistory) {
			if jsonOut {
				fmt.Fprintln(os.Stdout, "[]")
				return 0
			}
			fmt.Println("No history")
			return 0
		}
		fmt.Fprintln(os.Stderr, "history:", sanitizeError(err))
		return 3
	}
	profileID := historyProfileForPaths(configFile, managedDir)
	records, err := db.QueryProfile(since, route, networkLabel, profileID, profileID == history.DefaultProfile)
	if err != nil {
		closeErr := db.Close()
		if closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(records); err != nil {
			if closeErr := db.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close history: %w", closeErr))
			}
			fmt.Fprintln(os.Stderr, "encode history JSON:", sanitizeError(err))
			return 3
		}
		if err := db.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "history:", sanitizeError(err))
			return 3
		}
		return 0
	}
	if len(records) == 0 {
		if err := db.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "history:", sanitizeError(err))
			return 3
		}
		fmt.Println("No history")
		return 0
	}
	summaries := history.Summarize(records)
	fmt.Printf("\nLAST %s\n\n", strings.ToUpper(sinceStr))
	for _, s := range summaries {
		fmt.Printf("%s\n", s.Route)
		fmt.Printf("  Availability %6.1f%%\n", s.Availability)
		fmt.Printf("  Median       %6dms\n", s.Median.Milliseconds())
		if s.JitterKnown {
			fmt.Printf("  Jitter       %6dms\n", s.Jitter.Milliseconds())
		} else {
			fmt.Println("  Jitter       N/A")
		}
		fmt.Printf("  Failures     %6d\n", s.Failures)
		fmt.Printf("  Trend        %s\n", s.Trend)
		if s.Download > 0 {
			fmt.Printf("  Download     %.0f Mbps\n", s.Download)
		}
		fmt.Println()
	}
	// Most reliable
	best := ""
	bestAvail := -1.0
	for _, s := range summaries {
		if s.Availability > bestAvail {
			bestAvail = s.Availability
			best = s.Route
		}
	}
	if best != "" {
		fmt.Printf("Most reliable: %s\n", best)
	}
	if err := db.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "history:", sanitizeError(err))
		return 3
	}
	return 0
}

func parseSince(s string) (time.Duration, error) {
	// Support d for days
	if strings.HasSuffix(s, "d") {
		num := strings.TrimSuffix(s, "d")
		days, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return 0, err
		}
		if days < 0 || days > int64((1<<63-1)/(24*time.Hour)) {
			return 0, fmt.Errorf("days out of range")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must be non-negative")
	}
	return d, nil
}

// --- helpers ---
func newRecorder(cfg *config.Config, profileID string) *history.Recorder {
	enabled := cfg.History.Enabled == nil || *cfg.History.Enabled
	if profileID == "" {
		profileID = history.DefaultProfile
	}
	return history.NewRecorderForProfile(enabled, cfg.History.Retention.Duration, cfg.History.WatchInterval.Duration, profileID)
}

func probeRunQuick(cfg *config.Config) []probe.Result {
	ctx, cancel := context.WithTimeout(context.Background(), probeRunBudget(cfg))
	defer cancel()
	return probe.Run(ctx, cfg)
}

// probeRunBudget is the whole-operation budget for concurrently probing a
// config. Each endpoint keeps its own multi-sample contract; concurrent
// endpoints need only the slowest endpoint budget, not a fixed magic cap.
func probeRunBudget(cfg *config.Config) time.Duration {
	if cfg == nil || len(cfg.Endpoints) == 0 {
		return time.Second
	}
	budget := time.Second
	for _, endpoint := range cfg.Endpoints {
		if candidate := cfg.ProbeBudget(endpoint); candidate > budget {
			budget = candidate
		}
	}
	return budget
}

func networkMatrixRouteTimeout(cfg *config.Config) time.Duration {
	if cfg == nil {
		return time.Second
	}
	maximum := time.Second
	for _, endpoint := range cfg.Endpoints {
		if endpoint.Type != config.ProbeSingBox {
			continue
		}
		if timeout := cfg.TimeoutFor(endpoint); timeout > maximum {
			maximum = timeout
		}
	}
	return maximum
}

// recommendFromResults builds recommend inputs from probe results and
// enriches them with per-route history evidence when history is enabled.
func recommendFromResults(cfg *config.Config, results []probe.Result, configPath string, managedDir ...string) (recommend.Result, error) {
	return recommendFromResultsWithDownloads(cfg, results, nil, configPath, managedDir...)
}

func recommendFromResultsWithDownloads(cfg *config.Config, results []probe.Result, downloadFor func(string) float64, configPath string, managedDir ...string) (recommend.Result, error) {
	var inputs []recommend.Input
	for _, r := range results {
		if r.Endpoint.Type != config.ProbeSingBox {
			continue
		}
		in := recommend.Input{
			Name:        r.Endpoint.Name,
			Status:      string(r.Status),
			Loss:        r.Metrics.LossPct,
			Median:      r.Metrics.Median,
			Jitter:      r.Metrics.Jitter,
			JitterKnown: r.Metrics.JitterKnown,
			Priority:    r.Endpoint.Priority,
		}
		if downloadFor != nil {
			in.Download = downloadFor(r.Endpoint.Name)
		}
		inputs = append(inputs, in)
	}
	profileID := historyProfileForConfig(configPath)
	if len(managedDir) > 0 {
		profileID = historyProfileForPaths(configPath, managedDir[0])
	}
	rec := newRecorder(cfg, profileID)
	if rec.Enabled() {
		db, err := history.OpenReadonly()
		if err != nil {
			if !errors.Is(err, history.ErrNoHistory) {
				return recommend.Result{}, fmt.Errorf("open history: %w", err)
			}
		} else {
			recs, queryErr := db.QueryProfile(7*24*time.Hour, "", "", profileID, profileID == history.DefaultProfile)
			closeErr := db.Close()
			if queryErr != nil {
				return recommend.Result{}, fmt.Errorf("read history: %w", queryErr)
			}
			if closeErr != nil {
				return recommend.Result{}, fmt.Errorf("close history: %w", closeErr)
			}
			summaries := history.Summarize(recs)
			for i := range inputs {
				for _, s := range summaries {
					if s.Route == inputs[i].Name {
						inputs[i].Availability = s.Availability
						inputs[i].HistoryCount = s.Count
						if inputs[i].Download == 0 {
							inputs[i].Download = s.Download
						}
						break
					}
				}
			}
		}
	}
	return recommend.Recommend(inputs, 0), nil
}

func emit(results []probe.Result, asJSON bool, verbose bool) error {
	if asJSON {
		return report.JSON(os.Stdout, results)
	}
	if _, err := fmt.Fprintln(os.Stdout, report.Table(results)); err != nil {
		return err
	}
	if verbose {
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "debug %s: %s (category=%s)\n", r.Endpoint.Name, sanitizeError(r.Err), r.Category)
			}
		}
	}
	return nil
}

func watchJSON(ctx context.Context, cfg *config.Config, profileID string, interval time.Duration, verbose bool) (code int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	rec := newRecorder(cfg, profileID)
	defer func() {
		if err := rec.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "history:", sanitizeError(err))
			code = 3
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return 0
		default:
		}
		results := probe.Run(ctx, cfg)
		if ctx.Err() == nil {
			if _, err := rec.RecordThrottled(history.FromProbeResults(results, nil, "", profileID)); err != nil {
				fmt.Fprintf(os.Stderr, "history: %s\n", sanitizeError(err))
				return 3
			}
		}
		line, err := report.NDJSONLine(results)
		if err != nil {
			fmt.Fprintln(os.Stderr, "json encode:", sanitizeError(err))
			return 3
		} else {
			if _, err := fmt.Fprintln(os.Stdout, string(line)); err != nil {
				fmt.Fprintln(os.Stderr, "write JSON:", sanitizeError(err))
				return 3
			}
		}
		if verbose {
			for _, r := range results {
				if r.Err != nil {
					fmt.Fprintf(os.Stderr, "debug %s: %s\n", r.Endpoint.Name, sanitizeError(r.Err))
				}
			}
		}
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

func watchTUI(ctx context.Context, cfg *config.Config, profileID string, interval time.Duration, verbose bool) (code int) {
	rec := newRecorder(cfg, profileID)
	var recorderErr error
	defer func() {
		if err := rec.Close(); err != nil {
			recorderErr = errors.Join(recorderErr, err)
		}
		if recorderErr != nil {
			fmt.Fprintln(os.Stderr, "history:", sanitizeError(recorderErr))
			code = 3
		}
	}()
	observer := func(results []probe.Result) {
		if len(results) == 0 || ctx.Err() != nil {
			return
		}
		// Throttling (watch_interval) is owned by the recorder; manual
		// refreshes cannot flood the history DB.
		if _, err := rec.RecordThrottled(history.FromProbeResults(results, nil, "", profileID)); err != nil {
			recorderErr = errors.Join(recorderErr, err)
		}
	}
	m := tui.NewWithContext(ctx, cfg, interval, verbose, observer)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		if strings.Contains(err.Error(), "context canceled") {
			return 0
		}
		fmt.Fprintln(os.Stderr, "tui:", sanitizeError(err))
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
	if degraded > 0 || down > 0 {
		return 1
	}
	return 0
}

func hashFingerprint(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}
