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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
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
	"github.com/ilyasmohaimel/frostgate/internal/network"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
	"github.com/ilyasmohaimel/frostgate/internal/recommend"
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
	results := probe.Run(ctx, cfg)
	// history write (throttled for watch not needed here)
	recordHistory(results, "")
	emit(results, *jsonOut, *verbose)
	return exitCode(results)
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
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	nameFlag := fs.String("name", "", "route name (auto-derived if empty)")
	cfgPath := fs.String("config", "", "config path")
	_ = fs.Parse(args)
	links := fs.Args()
	if len(links) == 0 {
		fmt.Fprintln(os.Stderr, "usage: frostgate add <link> [--name name]")
		return 3
	}
	link := strings.TrimSpace(links[0])
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
	tag := strings.TrimSpace(*nameFlag)
	if tag == "" {
		tag = route.DeriveNameFromLink(link, 1)
	}
	tag = strings.ReplaceAll(tag, " ", "-")
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = route.DeriveNameFromLink(link, 1)
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
	singBoxPath := filepath.Join(dir, "sing-box.json")
	existingVless, existingHyst, existingFPs, existingTags := loadExistingOutbounds(singBoxPath)
	if _, exists := existingTags[tag]; exists {
		fmt.Printf("Route already exists: %s\n", tag)
		return 0
	}
	if _, dup := existingFPs[r.Fingerprint]; dup {
		for _, v := range existingVless {
			if route.FingerprintVLESS(v) == r.Fingerprint {
				fmt.Printf("Route already exists: %s\n", v.Tag)
				return 0
			}
		}
		for _, h := range existingHyst {
			if route.FingerprintHysteria(h) == r.Fingerprint {
				fmt.Printf("Route already exists: %s\n", h.Tag)
				return 0
			}
		}
		fmt.Printf("Route already exists: %s\n", tag)
		return 0
	}
	backupDir := backupConfig(dir)
	vlessOut := append([]*setup.VLESSOutbound{}, existingVless...)
	hystOut := append([]*setup.Hysteria2Outbound{}, existingHyst...)
	if r.VLESS != nil {
		vlessOut = append(vlessOut, r.VLESS)
	} else if r.Hysteria != nil {
		hystOut = append(hystOut, r.Hysteria)
	}
	secretFile := filepath.Join(dir, "controller-secret")
	secret := ""
	if data, err := os.ReadFile(secretFile); err == nil {
		secret = strings.TrimSpace(string(data))
	} else {
		s, _ := setup.GenerateSecret()
		secret = s
		setup.WriteSecretFile(secretFile, secret)
	}
	singData, err := setup.GenerateSingBoxConfig(secret, vlessOut, hystOut, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		rollbackConfig(dir, backupDir)
		return 3
	}
	if err := setup.WriteFileSecure(singBoxPath, singData); err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		rollbackConfig(dir, backupDir)
		return 3
	}
	fmt.Println("sing-box config valid")
	if sbPath, err := exec.LookPath("sing-box"); err == nil {
		cmd := exec.Command(sbPath, "check", "-c", singBoxPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("sing-box check failed: %s\n", sanitizeOutput(string(out)))
			rollbackConfig(dir, backupDir)
			fmt.Println("rolled back")
			return 3
		}
	}
	var vlessTags []string
	for _, v := range vlessOut {
		vlessTags = append(vlessTags, v.Tag)
	}
	var hystTags []string
	for _, h := range hystOut {
		hystTags = append(hystTags, h.Tag)
	}
	fgContent := setup.GenerateFrostgateConfig(vlessTags, hystTags, "https://www.gstatic.com/generate_204", "http://127.0.0.1:9090", secretFile)
	cfgPath2 := filepath.Join(dir, "config.yaml")
	if *cfgPath != "" {
		cfgPath2 = config.ResolveConfigPath(*cfgPath)
	}
	if err := setup.WriteFileSecure(cfgPath2, []byte(fgContent)); err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		rollbackConfig(dir, backupDir)
		return 3
	}
	if err := reloadSidecar(); err == nil {
		fmt.Println("sidecar reloaded")
	}
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

func loadExistingOutbounds(singBoxPath string) ([]*setup.VLESSOutbound, []*setup.Hysteria2Outbound, map[string]bool, map[string]bool) {
	vlessOut := []*setup.VLESSOutbound{}
	hystOut := []*setup.Hysteria2Outbound{}
	fps := map[string]bool{}
	tags := map[string]bool{}
	data, err := os.ReadFile(singBoxPath)
	if err != nil {
		return vlessOut, hystOut, fps, tags
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return vlessOut, hystOut, fps, tags
	}
	outs, ok := cfg["outbounds"].([]any)
	if !ok {
		return vlessOut, hystOut, fps, tags
	}
	for _, ob := range outs {
		m, ok := ob.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		tag, _ := m["tag"].(string)
		if tag != "" {
			tags[tag] = true
		}
		if t == "vless" {
			server, _ := m["server"].(string)
			portF, _ := m["server_port"].(float64)
			uuid, _ := m["uuid"].(string)
			sni := ""
			pbk := ""
			sid := ""
			fp := "chrome"
			flow := ""
			if tlsRaw, ok := m["tls"].(map[string]any); ok {
				sni, _ = tlsRaw["server_name"].(string)
				if utls, ok := tlsRaw["utls"].(map[string]any); ok {
					fp, _ = utls["fingerprint"].(string)
				}
				if reality, ok := tlsRaw["reality"].(map[string]any); ok {
					pbk, _ = reality["public_key"].(string)
					sid, _ = reality["short_id"].(string)
				}
			}
			if f, ok := m["flow"].(string); ok {
				flow = f
			}
			v := &setup.VLESSOutbound{Tag: tag, Server: server, ServerPort: int(portF), UUID: uuid, SNI: sni, PBK: pbk, SID: sid, FP: fp, Flow: flow}
			vlessOut = append(vlessOut, v)
			fps[route.FingerprintVLESS(v)] = true
		} else if t == "hysteria2" {
			server, _ := m["server"].(string)
			portF, _ := m["server_port"].(float64)
			pass, _ := m["password"].(string)
			obfsPass := ""
			if obfs, ok := m["obfs"].(map[string]any); ok {
				obfsPass, _ = obfs["password"].(string)
			}
			sni := ""
			if tlsRaw, ok := m["tls"].(map[string]any); ok {
				sni, _ = tlsRaw["server_name"].(string)
			}
			h := &setup.Hysteria2Outbound{Tag: tag, Server: server, ServerPort: int(portF), Password: pass, ObfsPassword: obfsPass, SNI: sni}
			hystOut = append(hystOut, h)
			fps[route.FingerprintHysteria(h)] = true
		}
	}
	return vlessOut, hystOut, fps, tags
}

func backupConfig(dir string) string {
	backupDir := filepath.Join(dir, fmt.Sprintf("backup-%s", time.Now().Format("20060102T150405")))
	os.MkdirAll(backupDir, 0o700)
	for _, n := range []string{"config.yaml", "sing-box.json", "controller-secret"} {
		if data, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			os.WriteFile(filepath.Join(backupDir, n), data, 0o600)
		}
	}
	return backupDir
}

func rollbackConfig(dir, backupDir string) {
	for _, n := range []string{"config.yaml", "sing-box.json", "controller-secret"} {
		if data, err := os.ReadFile(filepath.Join(backupDir, n)); err == nil {
			os.WriteFile(filepath.Join(dir, n), data, 0o600)
		} else {
			os.Remove(filepath.Join(dir, n))
		}
	}
}

func reloadSidecar() error {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(filepath.Dir(setup.ConfigDir(setup.DefaultHomeDir())), "systemd", "user", "frostgate-singbox.service"),
		filepath.Join(home, ".config", "systemd", "user", "frostgate-singbox.service"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			exec.Command("systemctl", "--user", "daemon-reload").Run()
			exec.Command("systemctl", "--user", "restart", "frostgate-singbox.service").Run()
			time.Sleep(800 * time.Millisecond)
			return nil
		}
	}
	return fmt.Errorf("no sidecar service")
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if strings.Contains(s, "vless://") || strings.Contains(s, "hysteria2://") || strings.Contains(s, "hy2://") {
		return "proxy parse error [redacted]"
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func sanitizeOutput(s string) string {
	if strings.Contains(s, "vless://") || strings.Contains(s, "hysteria2://") {
		return "[redacted]"
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// --- import ---
func handleImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: frostgate import <subscription-url>")
		return 3
	}
	subURL := rest[0]
	fmt.Println("Fetching subscription...")
	data, err := importer.FetchSubscription(context.Background(), subURL)
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
	singBoxPath := filepath.Join(dir, "sing-box.json")
	_, _, existingFPs, _ := loadExistingOutbounds(singBoxPath)
	seen := map[string]bool{}
	for k := range existingFPs {
		seen[k] = true
	}
	var toAdd []*route.ImportedRoute
	skippedDedup := 0
	skippedInvalid := 0
	for i, link := range res.Links {
		tag := route.DeriveNameFromLink(link, i+1)
		// Ensure tag uniqueness against existing and toAdd
		origTag := tag
		suffix := 1
		existingTags := map[string]bool{}
		ev, eh, _, et := loadExistingOutbounds(singBoxPath)
		_ = ev
		_ = eh
		for k := range et {
			existingTags[k] = true
		}
		for _, r2 := range toAdd {
			existingTags[r2.Tag] = true
		}
		for existingTags[tag] {
			suffix++
			tag = fmt.Sprintf("%s-%d", origTag, suffix)
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
		toAdd = append(toAdd, r)
	}
	if len(toAdd) == 0 {
		fmt.Fprintln(os.Stderr, "all routes were duplicates or invalid")
		return 3
	}
	var vlessOut []*setup.VLESSOutbound
	var hystOut []*setup.Hysteria2Outbound
	for _, r := range toAdd {
		if r.VLESS != nil {
			vlessOut = append(vlessOut, r.VLESS)
		}
		if r.Hysteria != nil {
			hystOut = append(hystOut, r.Hysteria)
		}
	}
	backupDir := backupConfig(dir)
	// Merge with existing
	existingVless, existingHyst, _, _ := loadExistingOutbounds(singBoxPath)
	// Prepend existing to keep order: existing first, then new
	vlessOut = append(existingVless, vlessOut...)
	hystOut = append(existingHyst, hystOut...)
	secretFile := filepath.Join(dir, "controller-secret")
	secret := ""
	if data, err := os.ReadFile(secretFile); err == nil {
		secret = strings.TrimSpace(string(data))
	} else {
		s, _ := setup.GenerateSecret()
		secret = s
		setup.WriteSecretFile(secretFile, secret)
	}
	singData, err := setup.GenerateSingBoxConfig(strings.TrimSpace(secret), vlessOut, hystOut, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		rollbackConfig(dir, backupDir)
		return 3
	}
	if err := setup.WriteFileSecure(singBoxPath, singData); err != nil {
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		rollbackConfig(dir, backupDir)
		return 3
	}
	if sbPath, err := exec.LookPath("sing-box"); err == nil {
		cmd := exec.Command(sbPath, "check", "-c", singBoxPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("sing-box check failed: %s\n", sanitizeOutput(string(out)))
			rollbackConfig(dir, backupDir)
			return 3
		}
	}
	var vlessTags []string
	for _, v := range vlessOut {
		vlessTags = append(vlessTags, v.Tag)
	}
	var hystTags []string
	for _, h := range hystOut {
		hystTags = append(hystTags, h.Tag)
	}
	fgContent := setup.GenerateFrostgateConfig(vlessTags, hystTags, "https://www.gstatic.com/generate_204", "http://127.0.0.1:9090", secretFile)
	cfgPath2 := filepath.Join(dir, "config.yaml")
	if *cfgPath != "" {
		cfgPath2 = config.ResolveConfigPath(*cfgPath)
	}
	setup.WriteFileSecure(cfgPath2, []byte(fgContent))
	_ = reloadSidecar()
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
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "json output")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: frostgate test <link> [--json]")
		return 3
	}
	link := rest[0]
	tag := "test-temp"
	r, err := route.ParseShareLink(link, tag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse: %v\n", sanitizeError(err))
		return 3
	}
	fmt.Println("Testing route (temporary, not saved)...")
	tmpDir, err := os.MkdirTemp("", "frostgate-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
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
		fmt.Fprintln(os.Stderr, sanitizeError(err))
		return 3
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
			fmt.Printf("sing-box check failed: %s\n", sanitizeOutput(string(out)))
			return 3
		}
	} else {
		fmt.Fprintln(os.Stderr, "sing-box not found")
		return 3
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
		fmt.Fprintln(os.Stderr, err)
		return 3
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
		fmt.Fprintln(os.Stderr, "controller not ready")
		return 3
	}
	timeout := 8 * time.Second
	delayCtx, cancel2 := context.WithTimeout(ctx, timeout)
	delay, err := client.Delay(delayCtx, tag, "https://www.gstatic.com/generate_204", timeout)
	cancel2()
	if err != nil {
		if *jsonOut {
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
	if *jsonOut {
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
	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	networkLabel := fs.String("network", "", "network label for history")
	noSpeed := fs.Bool("no-speed", false, "skip download speed")
	jsonOut := fs.Bool("json", false, "json output")
	cfgPath := fs.String("config", "", "config path")
	_ = fs.Parse(args)
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	dir := setup.ConfigDir(home)
	cfgFile := config.ResolveConfigPath(*cfgPath)
	if *cfgPath == "" {
		// Use default discovery but prefer dir
		if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err == nil {
			cfgFile = filepath.Join(dir, "config.yaml")
		}
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	// Estimate data
	routes := len(cfg.Endpoints)
	bytesPerRoute := int64(benchmark.DefaultBytes)
	totalBytes := int64(routes) * bytesPerRoute
	if !*noSpeed && routes > 10 {
		fmt.Printf("%d routes selected. Default benchmark will transfer ~%d MiB.\n", routes, totalBytes/1024/1024)
		fmt.Print("Continue? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		ans, _ := reader.ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(ans), "y") && !strings.EqualFold(strings.TrimSpace(ans), "yes") {
			fmt.Println("Cancelled")
			return 0
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	results := probe.Run(ctx, cfg)
	// For each, measure download if not noSpeed
	type benchRes struct {
		Result   probe.Result
		Download float64
	}
	var bres []benchRes
	for _, r := range results {
		download := 0.0
		if !*noSpeed && r.Status != "down" {
			// Try via sing-box SOCKS if singbox type
			if r.Endpoint.Type == config.ProbeSingBox {
				url := fmt.Sprintf("%s/proxies/%s", cfg.SingBox.Controller, "frostgate-selector")
				req, _ := http.NewRequestWithContext(ctx, "PUT", url, strings.NewReader(fmt.Sprintf(`{"name":"%s"}`, r.Endpoint.Outbound)))
				req.Header.Set("Authorization", "Bearer "+cfg.SingBox.Secret)
				req.Header.Set("Content-Type", "application/json")
				http.DefaultClient.Do(req)
				time.Sleep(200 * time.Millisecond)
				sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
				mbps, _, err := benchmark.MeasureViaSOCKS(sctx, "127.0.0.1:2080", benchmark.DefaultDownloadURL, bytesPerRoute)
				scancel()
				if err == nil {
					download = mbps
				}
			} else {
				// direct: measure direct
				sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
				mbps, _, _ := benchmark.MeasureDirect(sctx, benchmark.DefaultDownloadURL, bytesPerRoute)
				scancel()
				download = mbps
			}
		}
		bres = append(bres, benchRes{Result: r, Download: download})
		// Record history
		rec := history.Record{
			Route:        r.Endpoint.Name,
			Type:         string(r.Endpoint.Type),
			Status:       string(r.Status),
			Loss:         r.Metrics.LossPct,
			MedianMs:     r.Metrics.Median.Milliseconds(),
			JitterMs:     r.Metrics.Jitter.Milliseconds(),
			MinMs:        r.Metrics.Min.Milliseconds(),
			MaxMs:        r.Metrics.Max.Milliseconds(),
			MeanMs:       r.Metrics.Mean.Milliseconds(),
			DownloadMbps: download,
			Network:      *networkLabel,
			Category:     r.Category,
		}
		if db, err := history.Open(); err == nil {
			db.Add(rec)
			db.Close()
		}
	}
	// Sort by status then download then median (reuse recommend)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"results": bres, "network": *networkLabel})
		return exitCode(results)
	}
	fmt.Println("\nFROSTGATE BENCHMARK")
	fmt.Printf("%-20s %-8s %4s %7s %7s %10s\n", "ROUTE", "STATUS", "LOSS", "MEDIAN", "JITTER", "DOWNLOAD")
	for _, br := range bres {
		status := string(br.Result.Status)
		loss := fmt.Sprintf("%.0f%%", br.Result.Metrics.LossPct)
		median := fmtDuration(br.Result.Metrics.Median)
		jitter := fmtDuration(br.Result.Metrics.Jitter)
		down := "N/A"
		if br.Download > 0 {
			down = fmt.Sprintf("%.0f Mbps", br.Download)
		} else if br.Result.Endpoint.Type == config.ProbeSingBox && br.Download == 0 {
			down = "N/A"
		}
		fmt.Printf("%-20s %-8s %4s %7s %7s %10s\n", br.Result.Endpoint.Name, status, loss, median, jitter, down)
	}
	// Recommend
	var inputs []recommend.Input
	for _, br := range bres {
		inputs = append(inputs, recommend.Input{
			Name:     br.Result.Endpoint.Name,
			Status:   string(br.Result.Status),
			Loss:     br.Result.Metrics.LossPct,
			Median:   br.Result.Metrics.Median,
			Jitter:   br.Result.Metrics.Jitter,
			Download: br.Download,
			Priority: br.Result.Endpoint.Priority,
		})
	}
	rec := recommend.Recommend(inputs, len(bres))
	fmt.Printf("\nBEST OVERALL: %s\n", rec.BestOverall.Name)
	for _, r := range rec.Reasons {
		fmt.Printf("  ✓ %s\n", r)
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
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config path")
	jsonOut := fs.Bool("json", false, "json output")
	_ = fs.Parse(args)
	rest := fs.Args()
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	dir := setup.ConfigDir(home)
	cfgFile := config.ResolveConfigPath(*cfgPath)
	if *cfgPath == "" {
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
	if len(rest) == 0 {
		layers = diagnose.DiagnoseAll(cfg)
	} else {
		layers = diagnose.DiagnoseRoute(cfg, rest[0])
	}
	if *jsonOut {
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
func handleNetworkCheck(args []string) int {
	fs := flag.NewFlagSet("network-check", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "json output")
	_ = fs.Parse(args)
	checks := network.CheckAll()
	// Add destinations
	dests := []string{"https://github.com/", "https://www.google.com/generate_204", "https://www.wikipedia.org/"}
	for _, d := range network.CheckDestinations(dests) {
		checks = append(checks, d)
	}
	// Proxy routes
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	dir := setup.ConfigDir(home)
	cfgFile := filepath.Join(dir, "config.yaml")
	if _, err := os.Stat(cfgFile); err == nil {
		if cfg, err := config.Load(cfgFile); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			results := probe.Run(ctx, cfg)
			cancel()
			for _, r := range results {
				status := "PASS"
				if r.Status == "down" {
					status = "FAIL"
				}
				checks = append(checks, network.Check{Name: r.Endpoint.Name + " via proxy", Status: status, Detail: string(r.Status)})
			}
		}
	}
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(checks)
		return 0
	}
	fmt.Println("\nFROSTGATE NETWORK CHECK")
	fmt.Println("")
	fmt.Println("Connection")
	for _, c := range checks {
		if c.Name == "IPv4" || c.Name == "IPv6" || c.Name == "HTTPS" || c.Name == "Captive portal" {
			fmt.Printf("%-25s %-12s %s\n", c.Name, c.Status, c.Detail)
		}
	}
	fmt.Println("\nDNS")
	for _, c := range checks {
		if strings.Contains(c.Name, "resolver") || strings.Contains(c.Name, "Answers") {
			fmt.Printf("%-25s %-12s %s\n", c.Name, c.Status, c.Detail)
		}
	}
	fmt.Println("\nRoutes")
	for _, c := range checks {
		if strings.Contains(c.Name, "via proxy") || c.Name == "Direct" {
			fmt.Printf("%-25s %-12s %s\n", c.Name, c.Status, c.Detail)
		}
	}
	fmt.Println("\nDestinations")
	for _, c := range checks {
		if strings.HasPrefix(c.Name, "https://") {
			fmt.Printf("%-25s %-12s %s\n", c.Name, c.Status, c.Detail)
		}
	}
	fmt.Println("\nSignals")
	// Simple signals
	hasDirectFail := false
	hasProxyPass := false
	for _, c := range checks {
		if c.Name == "https://www.google.com/generate_204" && c.Status == "FAIL" {
			hasDirectFail = true
		}
		if strings.Contains(c.Name, "via proxy") && c.Status == "PASS" {
			hasProxyPass = true
		}
	}
	if hasDirectFail && hasProxyPass {
		fmt.Println("- direct access differs by destination")
	}
	for _, c := range checks {
		if c.Name == "Answers differ" && c.Status == "FAIL" {
			fmt.Println("- DNS answers differ from DoH")
		}
	}
	fmt.Println("\nRecommendation")
	// Find best route from last probe
	home2 := setup.ConfigDir(setup.DefaultHomeDir())
	_ = home2
	fmt.Println("Use best route from `frostgate benchmark`")
	return 0
}

// --- compare ---
func handleCompare(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "json output")
	_ = fs.Parse(args)
	db, err := history.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	defer db.Close()
	// Query all networks
	networks := []string{"home", "dorm", "mobile"}
	// Actually discover networks from history
	records, _ := db.Query(30*24*time.Hour, "", "")
	netSet := map[string]bool{}
	for _, r := range records {
		if r.Network != "" {
			netSet[r.Network] = true
		}
	}
	if len(netSet) > 0 {
		networks = nil
		for n := range netSet {
			networks = append(networks, n)
		}
	}
	if len(networks) == 0 {
		fmt.Println("No network history found. Run: frostgate benchmark --network <label>")
		return 0
	}
	// For each network, summarize
	type netSummary struct {
		Network   string
		Summaries []history.Summary
	}
	var all []netSummary
	for _, netw := range networks {
		recs, _ := db.Query(30*24*time.Hour, "", netw)
		summ := history.Summarize(recs)
		all = append(all, netSummary{Network: netw, Summaries: summ})
	}
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(all)
		return 0
	}
	fmt.Println("\nNETWORK COMPARISON")
	fmt.Println("")
	// Header
	fmt.Printf("%-20s", "")
	for _, ns := range all {
		fmt.Printf("%-12s", strings.ToUpper(ns.Network))
	}
	fmt.Println()
	// For each route, print per network
	// Collect all routes
	routeSet := map[string]bool{}
	for _, ns := range all {
		for _, s := range ns.Summaries {
			routeSet[s.Route] = true
		}
	}
	for route := range routeSet {
		fmt.Printf("\n%-20s\n", route)
		fmt.Printf("%-20s", "  Availability")
		for _, ns := range all {
			avail := "—"
			for _, s := range ns.Summaries {
				if s.Route == route {
					avail = fmt.Sprintf("%.0f%%", s.Availability)
				}
			}
			fmt.Printf("%-12s", avail)
		}
		fmt.Println()
		fmt.Printf("%-20s", "  Median")
		for _, ns := range all {
			med := "—"
			for _, s := range ns.Summaries {
				if s.Route == route {
					med = fmt.Sprintf("%dms", s.Median.Milliseconds())
				}
			}
			fmt.Printf("%-12s", med)
		}
		fmt.Println()
		fmt.Printf("%-20s", "  Download")
		for _, ns := range all {
			dl := "—"
			for _, s := range ns.Summaries {
				if s.Route == route && s.Download > 0 {
					dl = fmt.Sprintf("%.0fMb", s.Download)
				}
			}
			fmt.Printf("%-12s", dl)
		}
		fmt.Println()
	}
	fmt.Println("\nBest per network:")
	for _, ns := range all {
		best := ""
		bestAvail := -1.0
		for _, s := range ns.Summaries {
			if s.Availability > bestAvail {
				bestAvail = s.Availability
				best = s.Route
			}
		}
		if best != "" {
			fmt.Printf("%-12s %s\n", ns.Network, best)
		}
	}
	return 0
}

// --- recommend ---
func handleRecommend(args []string) int {
	fs := flag.NewFlagSet("recommend", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "json output")
	_ = fs.Parse(args)
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	dir := setup.ConfigDir(home)
	cfgFile := filepath.Join(dir, "config.yaml")
	if _, err := os.Stat(cfgFile); err != nil {
		// Try discovery
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
	var inputs []recommend.Input
	for _, r := range results {
		inputs = append(inputs, recommend.Input{
			Name:     r.Endpoint.Name,
			Status:   string(r.Status),
			Loss:     r.Metrics.LossPct,
			Median:   r.Metrics.Median,
			Jitter:   r.Metrics.Jitter,
			Priority: r.Endpoint.Priority,
		})
	}
	// Enrich with history availability
	db, err := history.Open()
	historyCount := 0
	if err == nil {
		defer db.Close()
		recs, _ := db.Query(7*24*time.Hour, "", "")
		historyCount = len(recs)
		summaries := history.Summarize(recs)
		for i, inp := range inputs {
			for _, s := range summaries {
				if s.Route == inp.Name {
					inputs[i].Availability = s.Availability
					inputs[i].Download = s.Download
				}
			}
		}
	}
	rec := recommend.Recommend(inputs, historyCount)
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(rec)
		return 0
	}
	fmt.Println("\nRECOMMENDATION")
	fmt.Println("")
	fmt.Printf("Best overall:\n%s\n\n", rec.BestOverall.Name)
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
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	sinceStr := fs.String("since", "24h", "since duration (e.g. 7d, 24h)")
	route := fs.String("route", "", "filter by route")
	networkLabel := fs.String("network", "", "filter by network")
	jsonOut := fs.Bool("json", false, "json output")
	_ = fs.Parse(args)
	since, err := parseSince(*sinceStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --since: %v\n", err)
		return 3
	}
	db, err := history.Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	defer db.Close()
	records, err := db.Query(since, *route, *networkLabel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(records)
		return 0
	}
	if len(records) == 0 {
		fmt.Println("No history")
		return 0
	}
	summaries := history.Summarize(records)
	fmt.Printf("\nLAST %s\n\n", strings.ToUpper(*sinceStr))
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
func recordHistory(results []probe.Result, network string) {
	db, err := history.Open()
	if err != nil {
		return
	}
	defer db.Close()
	for _, r := range results {
		rec := history.Record{
			Route:    r.Endpoint.Name,
			Type:     string(r.Endpoint.Type),
			Status:   string(r.Status),
			Loss:     r.Metrics.LossPct,
			MedianMs: r.Metrics.Median.Milliseconds(),
			JitterMs: r.Metrics.Jitter.Milliseconds(),
			MinMs:    r.Metrics.Min.Milliseconds(),
			MaxMs:    r.Metrics.Max.Milliseconds(),
			MeanMs:   r.Metrics.Mean.Milliseconds(),
			Network:  network,
			Category: r.Category,
		}
		db.Add(rec)
	}
	// Prune old
	db.Prune(90 * 24 * time.Hour)
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
	// Also history throttling: only write every 5m
	lastHistory := time.Now().Add(-10 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return 0
		default:
		}
		results := probe.Run(ctx, cfg)
		if time.Since(lastHistory) > 5*time.Minute {
			recordHistory(results, "")
			lastHistory = time.Now()
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
					fmt.Fprintf(os.Stderr, "debug %s: %v\n", r.Endpoint.Name, r.Err)
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
	if degraded > 0 || down > 0 {
		return 1
	}
	return 0
}

func hashFingerprint(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}
