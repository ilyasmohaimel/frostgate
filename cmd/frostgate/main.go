// Command frostgate probes self-hosted endpoints and ranks them by latency.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
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
	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/network"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
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

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			home := setup.DefaultHomeDir()
			if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
				home = h
			}
			if err := setup.RunInteractive(setup.Options{HomeDir: home}); err != nil {
				fmt.Fprintln(os.Stderr, "setup:", err)
				return 3
			}
			return 0
		case "doctor":
			fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
			cfgPath := fs.String("config", "", "path to config file")
			verbose := fs.Bool("verbose", false, "verbose output")
			_ = fs.Parse(os.Args[2:])
			path := config.ResolveConfigPath(*cfgPath)
			checks := doctor.Run(path, *verbose)
			fmt.Print(doctor.FormatColor(checks))
			for _, c := range checks {
				if c.Status == "FAIL" {
					return 1
				}
			}
			return 0
		case "add":
			return handleAdd(os.Args[2:])
		case "import":
			return handleImport(os.Args[2:])
		case "test":
			return handleTest(os.Args[2:])
		case "benchmark":
			return handleBenchmark(os.Args[2:])
		case "diagnose":
			return handleDiagnose(os.Args[2:])
		case "network-check", "network_check":
			return handleNetworkCheck(os.Args[2:])
		case "compare":
			return handleCompare(os.Args[2:])
		case "recommend":
			return handleRecommend(os.Args[2:])
		case "history":
			return handleHistory(os.Args[2:])
		case "help", "--help", "-h":
			printHelp()
			return 0
		}
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
	flag.Usage = func() { printHelp() }
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
	res := probe.Run(ctx, cfg)
	rec := newRecorder(cfg)
	defer rec.Close()
	_ = rec.Record(history.FromProbeResults(res, nil, ""))
	emit(res, *jsonOut, *verbose)
	return exitCode(res)
}

func printHelp() {
	fmt.Fprintf(os.Stderr, "frostgate %s — Network Route Sentinel\n\n", version)
	fmt.Fprintf(os.Stderr, "Usage:\n  frostgate [flags]\n  frostgate setup\n  frostgate add <link> [--name name]\n  frostgate import <url>\n  frostgate test <link>\n  frostgate benchmark [--network label] [--no-speed] [--json]\n  frostgate diagnose [route] [--json]\n  frostgate network-check [--json]\n  frostgate compare [--json]\n  frostgate recommend [--json]\n  frostgate history [--since duration] [--route name] [--network label] [--json]\n  frostgate doctor [--config path]\n\n")
	fmt.Fprintf(os.Stderr, "Core:\n  frostgate                 fast health check\n  frostgate --watch         live TUI\n  frostgate --watch --json  NDJSON\n\n")
	fmt.Fprintf(os.Stderr, "Setup:\n  frostgate setup                 wizard (creates private config)\n  frostgate add <link>            add one route (VLESS/Hysteria2)\n  frostgate import <url>          import subscription (one-time)\n  frostgate test <link>           temporary test, no save\n\n")
	fmt.Fprintf(os.Stderr, "Analysis:\n  frostgate benchmark             health + download speed\n  frostgate diagnose [route]      layered diagnosis\n  frostgate network-check         current network signals\n  frostgate compare               compare --network runs\n  frostgate recommend             explainable best route\n  frostgate history               local history (no secrets)\n\n")
	fmt.Fprintf(os.Stderr, "Diagnostics:\n  frostgate --doctor\n  frostgate --check\n  frostgate --version\n\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nExit codes: 0 all up  1 mixed/degraded  2 all down  3 config/error\n")
	fmt.Fprintf(os.Stderr, "\nConfig discovery (first existing wins):\n  1. --config path\n  2. $XDG_CONFIG_HOME/frostgate/config.yaml\n  3. ~/.config/frostgate/config.yaml\n  4. ./frostgate.yaml\n")
}

// --- add ---
func handleAdd(args []string) int {
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
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) == 0 {
		fmt.Fprintln(os.Stderr, "usage: frostgate add <link> [--name name]")
		return 3
	}
	link := strings.TrimSpace(positionals[0])
	if link == "" {
		fmt.Fprintln(os.Stderr, "empty link")
		return 3
	}
	fmt.Println("Importing route...")
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	dir := setup.ConfigDir(home)
	if err := setup.EnsureConfigDir(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	tag := strings.TrimSpace(nameFlag)
	if tag != "" {
		if err := route.ValidateName(tag); err != nil {
			fmt.Fprintf(os.Stderr, "invalid route name %q: %v\n", tag, sanitizeError(err))
			return 3
		}
	} else {
		tag = route.SanitizeName(route.DeriveNameFromLink(link, 1))
	}
	r, err := route.ParseShareLink(link, tag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse failed: %v\n", sanitizeError(err))
		return 3
	}
	displayProto := r.Protocol
	if r.Protocol == "vless" {
		displayProto = "VLESS + REALITY + Vision"
		if r.VLESS != nil && r.VLESS.PBK == "" {
			displayProto = "VLESS"
		}
	} else if r.Protocol == "hysteria2" {
		displayProto = "Hysteria2"
	}
	fmt.Printf("parsed %s\n", displayProto)
	mg, err := managedconfig.Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load managed config:", sanitizeError(err))
		return 3
	}
	for _, v := range mg.VLESS {
		if v.Tag == tag || route.FingerprintVLESS(v) == r.Fingerprint {
			fmt.Printf("Route already exists: %s\n", v.Tag)
			return 0
		}
	}
	for _, h := range mg.Hysteria {
		if h.Tag == tag || route.FingerprintHysteria(h) == r.Fingerprint {
			fmt.Printf("Route already exists: %s\n", h.Tag)
			return 0
		}
	}
	if r.VLESS != nil {
		mg.AddVLESS(r.VLESS)
	} else if r.Hysteria != nil {
		mg.AddHysteria(r.Hysteria)
	}
	cfgPath2 := filepath.Join(dir, "config.yaml")
	if cfgPath != "" {
		cfgPath2 = config.ResolveConfigPath(cfgPath)
	}
	controller := config.DefaultSingBoxController
	if existing, loadErr := config.Load(cfgPath2); loadErr == nil {
		controller = existing.SingBox.Controller
	}
	if mg.Secret == "" {
		mg.Secret, err = setup.GenerateSecret()
		if err != nil {
			fmt.Fprintln(os.Stderr, sanitizeError(err))
			return 3
		}
	}
	runner := managedconfig.DefaultSidecarRunner(controller, mg.Secret)
	if err := mg.SaveTransaction(runner); err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	fmt.Println("sing-box config valid")
	secretFile := filepath.Join(dir, "controller-secret")
	var vlessTags []string
	for _, v := range mg.VLESS {
		vlessTags = append(vlessTags, v.Tag)
	}
	var hystTags []string
	for _, h := range mg.Hysteria {
		hystTags = append(hystTags, h.Tag)
	}
	fgContent := setup.GenerateFrostgateConfig(vlessTags, hystTags, "https://www.gstatic.com/generate_204", controller, secretFile)
	if err := setup.WriteFileSecure(cfgPath2, []byte(fgContent)); err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	fmt.Println("sidecar reloaded and verified")
	fmt.Println("Testing new route...")
	cfg, err := config.Load(cfgPath2)
	if err != nil {
		fmt.Println("config load failed")
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
		ctx, cancel := context.WithTimeout(context.Background(), cfg.TimeoutFor(*ep))
		res := probe.ProbeEndpoint(ctx, cfg, *ep)
		cancel()
		if res.Status == "down" {
			fmt.Printf("end-to-end test failed: %s\n", sanitizeError(fmt.Errorf("%s", res.Category)))
			fmt.Println("Route added but test failed — check diagnose")
			return 1
		}
		fmt.Printf("\n%s\nLatency: %dms\nLoss: %.0f%%\nJitter: %dms\n", tag, res.Metrics.Median.Milliseconds(), res.Metrics.LossPct, res.Metrics.Jitter.Milliseconds())
	}
	fmt.Println("\nRoute added.")
	return 0
}

func sanitizeError(err error) string {
	return redact.Error(err)
}

func sanitizeOutput(s string) string {
	return redact.String(s)
}

// --- import ---
func handleImport(args []string) int {
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
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) == 0 {
		fmt.Fprintln(os.Stderr, "usage: frostgate import <subscription-url> [--allow-insecure-http]")
		return 3
	}
	subURL := positionals[0]
	fmt.Fprintln(os.Stderr, "Fetching subscription...")
	data, err := importer.FetchSubscriptionWithOpts(context.Background(), subURL, allowInsecure)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch failed: %v\n", sanitizeError(err))
		return 3
	}
	res := importer.ParseSubscription(data)
	if len(res.Links) == 0 {
		fmt.Fprintln(os.Stderr, "no usable routes found")
		for _, s := range res.Skipped {
			fmt.Fprintf(os.Stderr, "skip: %s\n", s.Reason)
		}
		return 3
	}
	fmt.Printf("Found %d links, %d skipped\n", len(res.Links), len(res.Skipped))
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	dir := setup.ConfigDir(home)
	setup.EnsureConfigDir(dir)
	// Deduplicate via fingerprint against existing and within subscription
	mg, err := managedconfig.Load(dir)
	if err != nil {
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
	cfgPath2 := filepath.Join(dir, "config.yaml")
	if cfgPath != "" {
		cfgPath2 = config.ResolveConfigPath(cfgPath)
	}
	controller := config.DefaultSingBoxController
	if existing, loadErr := config.Load(cfgPath2); loadErr == nil {
		controller = existing.SingBox.Controller
	}
	if mg.Secret == "" {
		mg.Secret, err = setup.GenerateSecret()
		if err != nil {
			fmt.Fprintln(os.Stderr, sanitizeError(err))
			return 3
		}
	}
	runner := managedconfig.DefaultSidecarRunner(controller, mg.Secret)
	if err := mg.SaveTransaction(runner); err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	secretFile := filepath.Join(dir, "controller-secret")
	var vlessTags []string
	for _, v := range mg.VLESS {
		vlessTags = append(vlessTags, v.Tag)
	}
	var hystTags []string
	for _, h := range mg.Hysteria {
		hystTags = append(hystTags, h.Tag)
	}
	fgContent := setup.GenerateFrostgateConfig(vlessTags, hystTags, "https://www.gstatic.com/generate_204", controller, secretFile)
	if err := setup.WriteFileSecure(cfgPath2, []byte(fgContent)); err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
	}
	fmt.Printf("Imported %d routes (%d skipped, %d deduped)\n", len(toAdd), len(res.Skipped)+skippedInvalid, skippedDedup)
	if len(res.Skipped) > 0 {
		fmt.Println("\nSkipped:")
		for _, s := range res.Skipped {
			fmt.Printf("- %s\n", s.Reason)
		}
	}
	cfg, err := config.Load(cfgPath2)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		results := probe.Run(ctx, cfg)
		cancel()
		fmt.Println("\nTESTING...")
		for i, r := range results {
			status := string(r.Status)
			fmt.Printf("%2d. %-20s %-8s %4.0f%% %6s\n", i+1, r.Endpoint.Name, status, r.Metrics.LossPct, fmtDuration(r.Metrics.Median))
		}
		usable := 0
		for _, r := range results {
			if r.Status != "down" {
				usable++
			}
		}
		fmt.Printf("\n%d/%d usable.\n", usable, len(results))
	}
	return 0
}

// --- test ---

func handleTest(args []string) int {
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
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	if len(positionals) == 0 {
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
	defer os.RemoveAll(tmpDir)
	os.Chmod(tmpDir, 0o700)
	secret, _ := setup.GenerateSecret()
	var vlessOut []*setup.VLESSOutbound
	var hystOut []*setup.Hysteria2Outbound
	if r.VLESS != nil {
		vlessOut = append(vlessOut, r.VLESS)
	}
	if r.Hysteria != nil {
		hystOut = append(hystOut, r.Hysteria)
	}
	port := freePort()
	mixedPort := freePort()
	if mixedPort == port {
		mixedPort = freePort()
	}
	controller := fmt.Sprintf("http://127.0.0.1:%d", port)
	singData, err := setup.GenerateSingBoxConfig(secret, vlessOut, hystOut, tmpDir)
	if err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", err, 3)
	}
	var cfgMap map[string]any
	json.Unmarshal(singData, &cfgMap)
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
	singData, _ = json.Marshal(cfgMap)
	singPath := filepath.Join(tmpDir, "sing-box.json")
	os.WriteFile(singPath, singData, 0o600)
	os.Chmod(singPath, 0o600)
	if sbPath, err := exec.LookPath("sing-box"); err == nil {
		cmd := exec.Command(sbPath, "check", "-c", singPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("sing-box check failed: %s", sanitizeOutput(string(out))), 3)
		}
	} else {
		return testCommandFailure(jsonOut, r.Protocol, "error", fmt.Errorf("sing-box not found"), 3)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	sbPath, _ := exec.LookPath("sing-box")
	cmd := exec.CommandContext(ctx, sbPath, "run", "-c", singPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return testCommandFailure(jsonOut, r.Protocol, "error", err, 3)
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cmd.Process.Kill()
			}
		}
		os.RemoveAll(tmpDir)
	}()
	client := singbox.New(controller, secret)
	var ready bool
	for i := 0; i < 25; i++ {
		time.Sleep(200 * time.Millisecond)
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
			json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol": strings.ToUpper(r.Protocol), "status": "down", "error": sanitizeError(err)})
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
		json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol": strings.ToUpper(r.Protocol), "status": "up", "median_ms": delay.Milliseconds(), "download_mbps": mbps})
	} else {
		fmt.Printf("Protocol: %s\nStatus:   HEALTHY\nLoss:     0%%\nMedian:   %dms\nJitter:   0ms\n", strings.ToUpper(r.Protocol), delay.Milliseconds())
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
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"protocol": strings.ToUpper(protocol),
			"status":   status,
			"error":    safe,
		})
	} else {
		fmt.Fprintln(os.Stderr, safe)
	}
	return code
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 2081
	}
	defer l.Close()
	addr := l.Addr().(*net.TCPAddr)
	return addr.Port
}

// --- benchmark ---
func handleBenchmark(args []string) int {
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
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	_ = positionals
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	dir := setup.ConfigDir(home)
	cfgFile := config.ResolveConfigPath(cfgPath)
	if cfgPath == "" {
		if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err == nil {
			cfgFile = filepath.Join(dir, "config.yaml")
		}
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	bytesPerRoute := cfg.Benchmark.DownloadBytes
	if bytesPerRoute == 0 {
		bytesPerRoute = benchmark.DefaultBytes
	}
	downloadURL := cfg.Benchmark.DownloadURL
	if downloadURL == "" {
		downloadURL = benchmark.DefaultDownloadURL
	}
	routes := len(cfg.Endpoints)
	totalBytes := int64(routes) * bytesPerRoute
	if !noSpeed && routes > 10 {
		msg := fmt.Sprintf("%d routes selected. Default benchmark will transfer ~%d MiB.\n", routes, totalBytes/1024/1024)
		if jsonOut {
			fmt.Fprint(os.Stderr, msg)
			fmt.Fprint(os.Stderr, "Continue? [y/N]: ")
		} else {
			fmt.Print(msg)
			fmt.Print("Continue? [y/N]: ")
		}
		reader := bufio.NewReader(os.Stdin)
		ans, _ := reader.ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(ans), "y") && !strings.EqualFold(strings.TrimSpace(ans), "yes") {
			if jsonOut {
				fmt.Fprintln(os.Stderr, "Cancelled")
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "cancelled", "results": []any{}})
			} else {
				fmt.Println("Cancelled")
			}
			return 0
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
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
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	proxyAddr := cfg.BenchmarkProxyAddr()
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
						br.DownloadNA = "benchmark_proxy not configured"
					}
				} else if probe.HasProxy(results) {
					br.DownloadNA = "direct baseline"
				} else {
					sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
					mbps, _, _ := benchmark.MeasureDirect(sctx, downloadURL, bytesPerRoute)
					scancel()
					br.Download = mbps
				}
				downloads[r.Endpoint.Name] = br.Download
				bres = append(bres, br)
			}
		default:
			for _, r := range results {
				if r.Endpoint.Type != config.ProbeSingBox {
					br := newBenchRes(r)
					if probe.HasProxy(results) {
						// Direct baseline is diagnostic only.
						br.DownloadNA = "direct baseline"
					} else {
						sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
						mbps, _, _ := benchmark.MeasureDirect(sctx, downloadURL, bytesPerRoute)
						scancel()
						br.Download = mbps
					}
					downloads[r.Endpoint.Name] = br.Download
					bres = append(bres, br)
				}
			}
			outboundToName := map[string]string{}
			var routes []string
			for _, r := range results {
				if r.Endpoint.Type == config.ProbeSingBox && r.Status != "down" {
					routes = append(routes, r.Endpoint.Outbound)
					outboundToName[r.Endpoint.Outbound] = r.Endpoint.Name
				}
			}
			// Attributed measurement: captures the selector, switches per
			// route, and restores it on success/error/cancellation.
			tp := benchmark.BenchRoutes(ctx, client, "frostgate-selector", proxyAddr, downloadURL, bytesPerRoute, routes, measure)
			tpByName := map[string]benchmark.RouteThroughput{}
			for _, t := range tp {
				name := outboundToName[t.Route]
				if name != "" {
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
	recCfg := newRecorder(cfg)
	defer recCfg.Close()
	if recCfg.Enabled() {
		_ = recCfg.Record(history.FromProbeResults(results, func(name string) float64 { return downloads[name] }, networkLabel))
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"schema_version": 3, "results": bres, "network": networkLabel})
		return exitCode(results)
	}
	fmt.Println("\nFROSTGATE BENCHMARK")
	if d, ok := probe.DirectBaseline(results); ok {
		lat := "—"
		if d.Metrics.Succeeded > 0 && d.Metrics.Median > 0 {
			lat = fmtDuration(d.Metrics.Median)
		}
		fmt.Printf("Direct baseline: %s %s\n", strings.ToUpper(string(d.Status)), lat)
	}
	fmt.Printf("%-20s %-8s %4s %7s %7s %10s\n", "ROUTE", "STATUS", "LOSS", "MEDIAN", "JITTER", "DOWNLOAD")
	for _, br := range bres {
		status := string(br.Result.Status)
		loss := fmt.Sprintf("%.0f%%", br.Result.Metrics.LossPct)
		median := fmtDuration(br.Result.Metrics.Median)
		jitter := fmtDuration(br.Result.Metrics.Jitter)
		down := fmt.Sprintf("%.0f Mbps", br.Download)
		if br.Download <= 0 {
			down = "N/A"
			if br.DownloadNA != "" {
				down = "N/A (" + br.DownloadNA + ")"
			}
		}
		fmt.Printf("%-20s %-8s %4s %7s %7s %10s\n", br.Result.Endpoint.Name, status, loss, median, jitter, down)
	}
	if bp, ok := probe.BestProxy(results); ok {
		fmt.Printf("\nBest proxy: %s (%s)\n", bp.Endpoint.Name, fmtDuration(bp.Metrics.Median))
	} else if probe.HasProxy(results) {
		fmt.Println("\nBest proxy: none usable")
	}
	rec := recommendFromResults(cfg, results, func(name string) float64 { return downloads[name] })
	if rec.BestOverall.Name != "" {
		fmt.Printf("\nBEST PROXY: %s\n", rec.BestOverall.Name)
		for _, r := range rec.Reasons {
			fmt.Printf("  ✓ %s\n", r)
		}
		if rec.Preferred != nil {
			fmt.Printf("\nPreferred configured route:\n%s\n", rec.Preferred.Name)
		}
	} else if probe.HasProxy(results) {
		fmt.Println("\nBest proxy: none configured")
	}
	return exitCode(results)
}

func fmtDuration(d time.Duration) string {
	if d == 0 {
		return "—"
	}
	if d < time.Millisecond {
		return "<1ms"
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// --- diagnose ---
func handleDiagnose(args []string) int {
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
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	rest := positionals
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	dir := setup.ConfigDir(home)
	cfgFile := config.ResolveConfigPath(cfgPath)
	if cfgPath == "" {
		if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err == nil {
			cfgFile = filepath.Join(dir, "config.yaml")
		}
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	var layers []diagnose.Layer
	opts := diagnose.Options{}
	if mg, err := managedconfig.Load(setup.ConfigDir(testHome())); err == nil {
		opts.Managed = mg
	}
	if len(rest) == 0 {
		layers = diagnose.DiagnoseAll(cfg, opts)
	} else {
		layers = diagnose.DiagnoseRoute(cfg, rest[0], opts)
	}
	if jsonOut {
		json.NewEncoder(os.Stdout).Encode(layers)
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
	hasFail := false
	for _, l := range layers {
		if l.Status == "FAIL" {
			hasFail = true
		}
	}
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
	return 0
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
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return 3
		}
	}

	doc := networkCheckDoc{SchemaVersion: 3}
	if os.Getenv("FROSTGATE_TEST_SKIP_BASE_NETWORK_CHECKS") != "1" {
		doc.Checks = network.CheckAll()
	}

	// Config-first destinations; defaults only when the list is empty.
	var destURLs []string
	destLabels := map[string]string{}
	var cfg *config.Config
	resolvedConfig := config.ResolveConfigPath(cfgPath)
	if loaded, err := config.Load(resolvedConfig); err == nil {
		cfg = loaded
	} else if cfgPath != "" {
		fmt.Fprintln(os.Stderr, err)
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
	if cfg != nil {
		mg, _ := managedconfig.Load(setup.ConfigDir(testHome()))
		routes := proxyRoutesForMatrix(cfg, mg)
		client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		doc.Matrix = network.DestinationMatrix(ctx, destURLs, routes, client, 6*time.Second)
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
		rec := recommendFromResults(cfg, results)
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
		json.NewEncoder(os.Stdout).Encode(doc)
		return 0
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
	if len(name) > 14 {
		return name[:13] + "~"
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

func testHome() string {
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		return h
	}
	return setup.DefaultHomeDir()
}

// defaultSidecarConfig returns the sidecar config path when it exists.
// --- compare ---
type compareRoute struct {
	Route        string  `json:"route"`
	Type         string  `json:"type,omitempty"`
	Availability float64 `json:"availability_pct"`
	Samples      int     `json:"samples"`
	MedianMs     int64   `json:"median_ms"`
	JitterMs     int64   `json:"jitter_ms"`
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
			DownloadMbps:    s.Download,
			HistoryCount:    s.Count,
			AvailabilityPct: s.Availability,
		})
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
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate compare [--json]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return 3
		}
	}
	db, err := history.OpenReadonly()
	if err != nil {
		msg := "No network history found. Run:\n  frostgate benchmark --network home\n"
		if jsonOut {
			fmt.Fprintln(os.Stderr, msg)
			json.NewEncoder(os.Stdout).Encode([]compareNetwork{})
			return 0
		}
		fmt.Print(msg)
		return 0
	}
	defer db.Close()
	records, _ := db.Query(30*24*time.Hour, "", "")
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
			json.NewEncoder(os.Stdout).Encode([]compareNetwork{})
			return 0
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
		recs, _ := db.Query(30*24*time.Hour, "", netw)
		summ := history.Summarize(recs)
		sort.Slice(summ, func(i, j int) bool { return summ[i].Route < summ[j].Route })
		cn := compareNetwork{Network: netw, Routes: make([]compareRoute, 0, len(summ))}
		for _, s := range summ {
			cn.Routes = append(cn.Routes, compareRoute{
				Route:        s.Route,
				Type:         s.Type,
				Availability: s.Availability,
				Samples:      s.Count,
				MedianMs:     s.Median.Milliseconds(),
				JitterMs:     s.Jitter.Milliseconds(),
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
		_ = enc.Encode(out)
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
			fmt.Printf("  %-12s avail %3.0f%%  samples %-4d median %4dms  jitter %3dms%s\n",
				cn.Network, cr.Availability, cr.Samples, cr.MedianMs, cr.JitterMs, dl)
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
	return 0
}

// --- recommend ---
func handleRecommend(args []string) int {
	jsonOut := false
	cfgPath := ""
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else if a == "--config" {
			// handled via next arg if needed, but recommend doesn't have config flag; ignore
		} else if strings.HasPrefix(a, "--config=") {
			cfgPath = strings.TrimPrefix(a, "--config=")
		} else if a == "--config" {
			// handled above
		} else if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "usage: frostgate recommend [--json] [--config path]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return 3
		}
	}
	// Also handle --config as second arg
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
		}
	}
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	dir := setup.ConfigDir(home)
	cfgFile := filepath.Join(dir, "config.yaml")
	if cfgPath != "" {
		cfgFile = config.ResolveConfigPath(cfgPath)
	} else if _, err := os.Stat(cfgFile); err != nil {
		cfgFile = config.ResolveConfigPath("")
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	results := probe.Run(ctx, cfg)
	cancel()
	rec := recommendFromResults(cfg, results)
	if jsonOut {
		json.NewEncoder(os.Stdout).Encode(rec)
		return 0
	}
	fmt.Println("\nRECOMMENDATION")
	fmt.Println("")
	if d, ok := probe.DirectBaseline(results); ok {
		lat := "—"
		if d.Metrics.Succeeded > 0 && d.Metrics.Median > 0 {
			lat = fmtDuration(d.Metrics.Median)
		}
		fmt.Printf("Direct baseline:\n%s %s\n\n", strings.ToUpper(string(d.Status)), lat)
	}
	if rec.BestOverall.Name == "" {
		fmt.Println("Best proxy:\nnone configured")
		fmt.Println("Confidence: insufficient data")
		return 0
	}
	fmt.Printf("Best proxy:\n%s\n\n", rec.BestOverall.Name)
	fmt.Println("Why:")
	for _, r := range rec.Reasons {
		fmt.Printf("✓ %s\n", r)
	}
	if rec.Preferred != nil {
		fmt.Printf("\nPreferred configured route:\n%s\n", rec.Preferred.Name)
	}
	fmt.Printf("\nFastest:\n%s\n", rec.Fastest.Name)
	fmt.Printf("\nMost stable:\n%s\n", rec.MostStable.Name)
	if len(rec.Avoid) > 0 {
		fmt.Println("\nAvoid:")
		for _, a := range rec.Avoid {
			fmt.Printf("- %s (%.0f%% loss)\n", a.Name, a.Loss)
		}
	}
	fmt.Printf("\nConfidence: %s\n%s\n", rec.Confidence, rec.ConfidenceDetail)
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
			fmt.Fprintln(os.Stderr, "usage: frostgate history [--since duration] [--route name] [--network label] [--json] [--config path]")
			return 0
		} else if len(a) > 0 && a[0] == '-' {
			fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
			return 3
		} else {
			positionals = append(positionals, a)
		}
	}
	_ = positionals
	_ = cfgPath
	since, err := parseSince(sinceStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --since: %v\n", err)
		return 3
	}
	db, err := history.OpenReadonly()
	if err != nil {
		if jsonOut {
			fmt.Fprintln(os.Stdout, "[]")
			return 0
		}
		fmt.Println("No history")
		return 0
	}
	defer db.Close()
	records, err := db.Query(since, route, networkLabel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if jsonOut {
		json.NewEncoder(os.Stdout).Encode(records)
		return 0
	}
	if len(records) == 0 {
		fmt.Println("No history")
		return 0
	}
	summaries := history.Summarize(records)
	fmt.Printf("\nLAST %s\n\n", strings.ToUpper(sinceStr))
	for _, s := range summaries {
		fmt.Printf("%s\n", s.Route)
		fmt.Printf("  Availability %6.1f%%\n", s.Availability)
		fmt.Printf("  Median       %6dms\n", s.Median.Milliseconds())
		fmt.Printf("  Jitter       %6dms\n", s.Jitter.Milliseconds())
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
	return 0
}

func parseSince(s string) (time.Duration, error) {
	// Support d for days
	if strings.HasSuffix(s, "d") {
		num := strings.TrimSuffix(s, "d")
		n, err := fmt.Sscanf(num, "%d", new(int))
		_ = n
		if err == nil {
			// parse via int
			var days int
			fmt.Sscanf(num, "%d", &days)
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}

// --- helpers ---
func newRecorder(cfg *config.Config) *history.Recorder {
	enabled := cfg.History.Enabled == nil || *cfg.History.Enabled
	return history.NewRecorder(enabled, cfg.History.Retention.Duration, cfg.History.WatchInterval.Duration)
}

func probeRunQuick(cfg *config.Config) []probe.Result {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return probe.Run(ctx, cfg)
}

// recommendFromResults builds recommend inputs from probe results and
// enriches them with per-route history evidence when history is enabled.
func recommendFromResults(cfg *config.Config, results []probe.Result, downloadFor ...func(string) float64) recommend.Result {
	var inputs []recommend.Input
	for _, r := range results {
		if r.Endpoint.Type != config.ProbeSingBox {
			continue
		}
		in := recommend.Input{
			Name:     r.Endpoint.Name,
			Status:   string(r.Status),
			Loss:     r.Metrics.LossPct,
			Median:   r.Metrics.Median,
			Jitter:   r.Metrics.Jitter,
			Priority: r.Endpoint.Priority,
		}
		if len(downloadFor) > 0 && downloadFor[0] != nil {
			in.Download = downloadFor[0](r.Endpoint.Name)
		}
		inputs = append(inputs, in)
	}
	rec := newRecorder(cfg)
	if rec.Enabled() {
		if db, err := history.OpenReadonly(); err == nil {
			defer db.Close()
			recs, _ := db.Query(7*24*time.Hour, "", "")
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
	return recommend.Recommend(inputs, 0)
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
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	rec := newRecorder(cfg)
	defer rec.Close()
	for {
		select {
		case <-ctx.Done():
			return 0
		default:
		}
		results := probe.Run(ctx, cfg)
		if _, err := rec.RecordThrottled(history.FromProbeResults(results, nil, "")); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "history: %v\n", err)
		}
		line, err := report.NDJSONLine(results)
		if err != nil {
			fmt.Fprintln(os.Stderr, "json encode:", err)
		} else {
			fmt.Println(string(line))
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

func watchTUI(ctx context.Context, cfg *config.Config, interval time.Duration, verbose bool) int {
	rec := newRecorder(cfg)
	defer rec.Close()
	observer := func(results []probe.Result) {
		if len(results) == 0 {
			return
		}
		// Throttling (watch_interval) is owned by the recorder; manual
		// refreshes cannot flood the history DB.
		if _, err := rec.RecordThrottled(history.FromProbeResults(results, nil, "")); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "history: %v\n", err)
		}
	}
	m := tui.New(cfg, interval, verbose, observer)
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
	if degraded > 0 || down > 0 {
		return 1
	}
	return 0
}

func hashFingerprint(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}
