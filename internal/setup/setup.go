package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/redact"
	"github.com/ilyasmohaimel/frostgate/internal/routename"
)

type Options struct {
	HomeDir string
	Stdin   *bufio.Reader
	Stdout  *os.File
	Stderr  *os.File
	// AddRoute lets the application layer apply its managed-config transaction
	// when setup adds a route to an existing configuration.
	AddRoute func(dir, link, name string) error
}

var backupNow = time.Now

func DefaultHomeDir() string {
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	hd, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return hd
}

func ConfigDir(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "frostgate")
	}
	return filepath.Join(home, ".config", "frostgate")
}

func RunInteractive(opts Options) error {
	if opts.HomeDir == "" {
		opts.HomeDir = DefaultHomeDir()
	}
	if opts.Stdin == nil {
		opts.Stdin = bufio.NewReader(os.Stdin)
	}
	dir := ConfigDir(opts.HomeDir)
	fmt.Printf("\nFrostgate setup\n\n")
	fmt.Printf("Config directory: %s\n", dir)
	fmt.Printf("OS: %s  Arch: %s\n\n", runtime.GOOS, runtime.GOARCH)

	// Preflight: existing config?
	existingConfig := filepath.Join(dir, "config.yaml")
	hasExisting := false
	var err error
	if _, err := os.Stat(existingConfig); err == nil {
		hasExisting = true
	}
	var transaction *setupTransaction
	if hasExisting {
		fmt.Printf("Existing Frostgate config found at %s\n", existingConfig)
		fmt.Printf("[a] Add route  [r] Reconfigure  [x] Reset  [c] Cancel\n")
		choice, err := prompt(opts.Stdin, "Choose [a/r/x/c] (default c): ")
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "a":
			return addRouteFlow(opts, dir)
		case "r":
			fmt.Println("Reconfiguring; existing config will be backed up")
			if err := backupExisting(dir); err != nil {
				return err
			}
			transaction, err = beginSetupTransaction(dir)
			if err != nil {
				return err
			}
		case "x":
			confirm, err := prompt(opts.Stdin, "Type YES to reset and delete existing config: ")
			if err != nil {
				return err
			}
			if confirm != "YES" {
				fmt.Println("Cancelled")
				return nil
			}
			if err := backupExisting(dir); err != nil {
				return err
			}
			transaction, err = beginSetupTransaction(dir)
			if err != nil {
				return err
			}
			fmt.Println("Existing setup will be replaced transactionally")
		default:
			fmt.Println("Cancelled; no changes made")
			return nil
		}
	}

	// Ensure dir with 0700
	if err := EnsureConfigDir(dir); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	if transaction == nil {
		transaction, err = beginSetupTransaction(dir)
		if err != nil {
			return err
		}
	}
	fmt.Printf("config directory ready: %s (0700)\n", dir)

	// Choose setup type
	fmt.Println("\nHow do you want to configure Frostgate?")
	fmt.Println("  1) Import proxy route(s) (VLESS/Hysteria2 share links)")
	fmt.Println("  2) Use existing sing-box controller (advanced)")
	fmt.Println("  3) Direct internet checks only (no proxy)")
	choice, err := prompt(opts.Stdin, "Choose [1/2/3] (default 1): ")
	if err != nil {
		return err
	}
	if choice == "" {
		choice = "1"
	}
	var setupErr error
	switch choice {
	case "1":
		sbPath, err := prepareSingBoxForImport(opts)
		if err != nil {
			setupErr = err
			break
		}
		setupErr = importFlow(opts, dir, sbPath, transaction)
	case "2":
		setupErr = existingControllerFlow(opts, dir)
	case "3":
		setupErr = directOnlyFlow(opts, dir)
	default:
		fmt.Println("Invalid choice, defaulting to import")
		sbPath, err := prepareSingBoxForImport(opts)
		if err != nil {
			setupErr = err
			break
		}
		setupErr = importFlow(opts, dir, sbPath, transaction)
	}
	if setupErr != nil {
		return transaction.rollback(setupErr)
	}
	cfg, err := config.Load(existingConfig)
	if err != nil {
		return transaction.rollback(fmt.Errorf("validate candidate Frostgate config: %w", err))
	}
	if err := transaction.finish(hasProxyEndpoints(cfg)); err != nil {
		return transaction.rollback(err)
	}
	return nil
}

// prepareSingBoxForImport checks for the proxy engine only after the user
// selected a proxy-import flow. Direct-only and external-controller setup do
// not need it and must not prompt for, install, or configure sing-box.
func prepareSingBoxForImport(opts Options) (string, error) {
	sbPath, err := exec.LookPath("sing-box")
	if err != nil {
		fmt.Println("\nsing-box not found in PATH")
		fmt.Println("Frostgate uses sing-box as the protocol engine (no proxy protocols are reimplemented in Frostgate).")
		install, promptErr := prompt(opts.Stdin, "Install sing-box now? [Y/n]: ")
		if promptErr != nil {
			return "", promptErr
		}
		if !isNo(install) {
			sbPath, err = tryInstallSingBox(opts)
			if err != nil {
				fmt.Printf("Automatic install failed: %v\n", redact.Error(err))
				fmt.Println("Please install sing-box manually: https://sing-box.sagernet.org/installation/")
				return "", err
			}
		} else {
			fmt.Println("Continuing without sing-box. Proxy routes cannot be tested until it is installed.")
		}
	}
	if sbPath != "" {
		ver := getSingBoxVersion(sbPath)
		fmt.Printf("sing-box found: %s (%s)\n", ver, sbPath)
	}
	return sbPath, nil
}

func prompt(r *bufio.Reader, msg string) (string, error) {
	fmt.Print(msg)
	s, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("setup input ended unexpectedly")
		}
		return "", fmt.Errorf("read setup input: %w", err)
	}
	return strings.TrimSpace(s), nil
}

func isNo(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "n" || s == "no"
}

func backupExisting(dir string) error {
	ts := backupNow().Format("20060102T150405")
	backupBase := filepath.Join(dir, "backup-"+ts)
	backupDir := ""
	for suffix := 0; suffix < 1000; suffix++ {
		candidate := backupBase
		if suffix > 0 {
			candidate = fmt.Sprintf("%s-%d", backupBase, suffix)
		}
		err := os.Mkdir(candidate, 0o700)
		if err == nil {
			backupDir = candidate
			break
		}
		if !os.IsExist(err) {
			return fmt.Errorf("create backup: %w", err)
		}
	}
	if backupDir == "" {
		return fmt.Errorf("create backup: too many backups with timestamp %s", ts)
	}
	for _, name := range []string{"config.yaml", "sing-box.json", "controller-secret"} {
		src := filepath.Join(dir, name)
		data, err := os.ReadFile(src)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s for backup: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(backupDir, name), data, 0o600); err != nil {
			return fmt.Errorf("write %s backup: %w", name, err)
		}
	}
	fmt.Printf("Backup saved to %s\n", backupDir)
	return nil
}

func tryInstallSingBox(opts Options) (string, error) {
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("pacman"); err == nil {
			fmt.Println("Attempting: sudo pacman -S --noconfirm sing-box")
			cmd := exec.Command("sudo", "pacman", "-S", "--noconfirm", "sing-box")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return "", err
			}
			path, err := exec.LookPath("sing-box")
			if err != nil {
				return "", fmt.Errorf("sing-box was installed but is not in PATH: %w", err)
			}
			return path, nil
		}
		asset, err := SingBoxAssetFor(runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return "", err
		}
		url, err := SingBoxReleaseURL(runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return "", err
		}
		fmt.Println("No pacman found. Downloading a verified static binary from GitHub...")
		tmpDir, err := os.MkdirTemp("", "sing-box-install-")
		if err != nil {
			return "", fmt.Errorf("create temporary directory: %w", err)
		}
		defer os.RemoveAll(tmpDir)
		fmt.Printf("Downloading %s ...\n", url)
		archive := filepath.Join(tmpDir, asset.Name)
		var cmd *exec.Cmd
		if _, err := exec.LookPath("curl"); err == nil {
			cmd = exec.Command("curl", "-fL", url, "-o", archive)
		} else if _, err := exec.LookPath("wget"); err == nil {
			cmd = exec.Command("wget", "-qO", archive, url)
		} else {
			return "", fmt.Errorf("neither curl nor wget found")
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", err
		}
		if err := VerifySHA256(archive, asset.SHA256); err != nil {
			return "", fmt.Errorf("download verification failed: %w", err)
		}
		ex := exec.Command("tar", "xzf", archive, "-C", tmpDir)
		if err := ex.Run(); err != nil {
			return "", err
		}
		bin := filepath.Join(tmpDir, strings.TrimSuffix(asset.Name, ".tar.gz"), "sing-box")
		if _, err := os.Stat(bin); err != nil {
			out, findErr := exec.Command("find", tmpDir, "-name", "sing-box", "-type", "f").Output()
			if findErr != nil {
				return "", fmt.Errorf("find sing-box binary in verified archive: %w", findErr)
			}
			matches := strings.Fields(string(out))
			if len(matches) != 1 {
				return "", fmt.Errorf("sing-box binary not found in verified archive")
			}
			bin = matches[0]
		}
		if err := exec.Command(bin, "version").Run(); err != nil {
			return "", fmt.Errorf("downloaded sing-box failed version check: %w", err)
		}
		installDir := filepath.Join(opts.HomeDir, ".local", "bin")
		if err := os.MkdirAll(installDir, 0o755); err != nil {
			return "", fmt.Errorf("create install directory: %w", err)
		}
		dest := filepath.Join(installDir, "sing-box")
		data, err := os.ReadFile(bin)
		if err != nil {
			return "", fmt.Errorf("read downloaded binary: %w", err)
		}
		if err := writeFileAtomic(dest, data, 0o755); err != nil {
			return "", fmt.Errorf("install sing-box: %w", err)
		}
		fmt.Printf("Installed sing-box to %s\n", dest)
		// check PATH
		if !isInPath(installDir) {
			fmt.Printf("NOTE: %s not in PATH. Add: export PATH=\"$HOME/.local/bin:$PATH\"\n", installDir)
		}
		return dest, nil
	default:
		return "", fmt.Errorf("automatic install not supported on %s; see https://sing-box.sagernet.org/installation/", runtime.GOOS)
	}
}

func isInPath(dir string) bool {
	for _, p := range strings.Split(os.Getenv("PATH"), ":") {
		if p == dir {
			return true
		}
	}
	return false
}

func getSingBoxVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown"
	}
	s := strings.TrimSpace(string(out))
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		return "unknown"
	}
	// first line
	if idx := strings.Index(s, "\n"); idx != -1 {
		s = s[:idx]
	}
	return s
}

func importFlow(opts Options, dir, singBoxPath string, transaction *setupTransaction) error {
	var vlessOut []*VLESSOutbound
	var hystOut []*Hysteria2Outbound

	fmt.Println("\nImport proxy routes")
	fmt.Println("Paste your share links (vless:// or hysteria2:// / hy2://)")
	fmt.Println("Leave empty to finish. Your input is not logged.")
	for {
		link, err := prompt(opts.Stdin, "\nPaste share link (empty to finish): ")
		if err != nil {
			return err
		}
		if link == "" {
			if len(vlessOut)+len(hystOut) == 0 {
				// allow direct only fallback
				fmt.Println("No routes added yet.")
				cont, err := prompt(opts.Stdin, "Add direct baseline instead? [Y/n]: ")
				if err != nil {
					return err
				}
				if !isNo(cont) {
					break
				}
				continue
			}
			break
		}
		tag, err := prompt(opts.Stdin, "Name for this route (e.g. reality-primary): ")
		if err != nil {
			return err
		}
		if tag == "" {
			fmt.Println("Name required")
			continue
		}
		if err := routename.Validate(tag); err != nil {
			fmt.Printf("Invalid route name: %s\n", redact.Error(err))
			continue
		}
		linkLower := strings.ToLower(strings.TrimSpace(link))
		if strings.HasPrefix(linkLower, "vless://") {
			v, err := ParseVLESSShareLink(link, tag)
			if err != nil {
				fmt.Printf("Failed to parse VLESS: %s (redacted: %s)\n", redact.Error(err), RedactURL(link))
				continue
			}
			vlessOut = append(vlessOut, v)
			fmt.Printf("added VLESS: %s\n", tag)
		} else if strings.HasPrefix(linkLower, "hysteria2://") || strings.HasPrefix(linkLower, "hy2://") {
			h, err := ParseHysteria2ShareLink(link, tag)
			if err != nil {
				fmt.Printf("Failed to parse Hysteria2: %s (redacted: %s)\n", redact.Error(err), RedactURL(link))
				continue
			}
			hystOut = append(hystOut, h)
			fmt.Printf("added Hysteria2: %s\n", tag)
		} else {
			fmt.Printf("Unsupported link (must be vless:// or hysteria2://)\n")
			continue
		}
		more, err := prompt(opts.Stdin, "Add another route? [y/N]: ")
		if err != nil {
			return err
		}
		if !strings.EqualFold(more, "y") && !strings.EqualFold(more, "yes") {
			if more == "" {
				break
			}
			if isNo(more) {
				break
			}
		}
	}
	// Direct baseline offer
	testURL := "https://www.gstatic.com/generate_204"
	useDirect, err := prompt(opts.Stdin, fmt.Sprintf("Add direct baseline (%s)? [Y/n]: ", testURL))
	if err != nil {
		return err
	}
	addDirect := true
	if isNo(useDirect) {
		custom, err := prompt(opts.Stdin, "Custom test URL (empty to skip direct): ")
		if err != nil {
			return err
		}
		if custom != "" {
			testURL = custom
			addDirect = true
		} else {
			addDirect = false
		}
	}
	endpointCount := len(vlessOut) + len(hystOut)
	if addDirect {
		endpointCount++
	}
	if err := config.ValidateEndpointCount(endpointCount); err != nil {
		return err
	}
	if len(vlessOut)+len(hystOut) == 0 {
		if !addDirect {
			return fmt.Errorf("at least one route or direct baseline is required")
		}
		content, err := yaml.Marshal(BuildDirectOnlyConfig(testURL))
		if err != nil {
			return fmt.Errorf("encode direct-only Frostgate config: %w", err)
		}
		fgPath := filepath.Join(dir, "config.yaml")
		if err := WriteFileSecure(fgPath, content); err != nil {
			return err
		}
		fmt.Printf("wrote %s (direct only)\n", fgPath)
		return runValidation(dir)
	}

	// Controller secret
	secretFile := filepath.Join(dir, "controller-secret")
	var secret string
	if data, err := os.ReadFile(secretFile); err == nil {
		secret = strings.TrimSpace(string(data))
		fmt.Println("using existing controller secret")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing controller secret: %w", err)
	} else {
		s, err := GenerateSecret()
		if err != nil {
			return err
		}
		secret = s
		if err := WriteSecretFile(secretFile, secret); err != nil {
			return err
		}
		fmt.Println("generated controller secret (0600)")
	}

	// Generate sing-box config
	singBoxData, err := GenerateSingBoxConfig(secret, vlessOut, hystOut, dir)
	if err != nil {
		return err
	}
	singBoxConfigPath := filepath.Join(dir, "sing-box.json")
	if err := WriteFileSecure(singBoxConfigPath, singBoxData); err != nil {
		return err
	}
	fmt.Printf("wrote %s (0600)\n", singBoxConfigPath)

	// Validate sing-box
	if singBoxPath != "" {
		cmd := exec.Command(singBoxPath, "check", "-c", singBoxConfigPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("sing-box check failed: %s\n", redact.String(string(out)))
			return fmt.Errorf("sing-box check failed")
		}
		fmt.Println("sing-box check passed")
	}

	// Generate Frostgate config
	var vlessTags []string
	for _, v := range vlessOut {
		vlessTags = append(vlessTags, v.Tag)
	}
	var hystTags []string
	for _, h := range hystOut {
		hystTags = append(hystTags, h.Tag)
	}
	controller := "http://127.0.0.1:9090"
	secretFileForConfig := secretFile
	fgConfig := BuildFrostgateConfig(vlessTags, hystTags, testURL, controller, secretFileForConfig)
	if !addDirect {
		fgConfig.Endpoints = fgConfig.Endpoints[1:]
		if len(fgConfig.Endpoints) == 0 {
			return fmt.Errorf("at least one route or direct baseline is required")
		}
	}
	fgContent, err := yaml.Marshal(fgConfig)
	if err != nil {
		return fmt.Errorf("encode Frostgate config: %w", err)
	}
	fgPath := filepath.Join(dir, "config.yaml")
	if err := WriteFileSecure(fgPath, fgContent); err != nil {
		return err
	}
	fmt.Printf("wrote %s (0600)\n", fgPath)

	// Validate the full candidate before it can replace an active managed
	// sidecar. A requested service installation is an activation step, not a
	// substitute for candidate validation.
	fmt.Println("\nValidating...")
	if err := runValidation(dir, singBoxPath); err != nil {
		fmt.Printf("Validation failed: %s\n", redact.Error(err))
		return err
	}

	// Sidecar service
	if runtime.GOOS == "linux" && singBoxPath != "" {
		offer, err := prompt(opts.Stdin, "Install systemd user service for sing-box sidecar? [Y/n]: ")
		if err != nil {
			return err
		}
		if !isNo(offer) {
			if err := installSystemdService(dir, singBoxPath, transaction); err != nil {
				return fmt.Errorf("install requested systemd service: %w", err)
			}
			fmt.Println("systemd service installed and started")
		}
	}

	fmt.Println("\nFrostgate setup complete.")
	fmt.Println("\nRoutes:")
	for _, t := range vlessTags {
		fmt.Printf("  %s\n", t)
	}
	for _, t := range hystTags {
		fmt.Printf("  %s\n", t)
	}
	if addDirect {
		fmt.Printf("  direct\n")
	}
	fmt.Printf("\nsing-box: run `sing-box run -c %s` if no service was installed\n", singBoxConfigPath)
	fmt.Println("Controller: loopback only")
	fmt.Printf("Config: %s\n", fgPath)
	fmt.Println("\nTry:")
	fmt.Println("  frostgate")
	fmt.Println("  frostgate --watch")
	fmt.Println("  frostgate --doctor")
	return nil
}

func existingControllerFlow(opts Options, dir string) error {
	fmt.Println("\nUse existing sing-box controller")
	controller, err := prompt(opts.Stdin, "Controller URL [http://127.0.0.1:9090]: ")
	if err != nil {
		return err
	}
	if controller == "" {
		controller = "http://127.0.0.1:9090"
	}
	secret, err := prompt(opts.Stdin, "Controller secret (leave empty if none): ")
	if err != nil {
		return err
	}
	secretFile := ""
	secretToSave := ""
	if secret != "" {
		// offer to save to file
		save, err := prompt(opts.Stdin, "Save secret to controller-secret file? [Y/n]: ")
		if err != nil {
			return err
		}
		if !isNo(save) {
			secretFile = filepath.Join(dir, "controller-secret")
			secretToSave = secret
			secret = "" // use file
		}
	}
	// Ask for outbound tags
	fmt.Println("Enter your existing sing-box outbound tags (comma or space separated)")
	tagsStr, err := prompt(opts.Stdin, "Tags (e.g. reality,hysteria): ")
	if err != nil {
		return err
	}
	var tags []string
	for _, t := range strings.FieldsFunc(tagsStr, func(r rune) bool { return r == ',' || r == ' ' }) {
		if t != "" {
			if err := routename.Validate(t); err != nil {
				return fmt.Errorf("invalid route name %q: %w", t, err)
			}
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return fmt.Errorf("no tags provided")
	}
	if err := config.ValidateEndpointCount(len(tags) + 1); err != nil {
		return err
	}
	if secretToSave != "" {
		if err := WriteSecretFile(secretFile, secretToSave); err != nil {
			return err
		}
	}
	testURL := config.DefaultTestURL
	fgConfig := BuildExternalControllerConfig(tags, testURL, controller, secretFile)
	if secretFile == "" && secret != "" {
		fgConfig.SingBox.Secret = secret
	}
	fgContent, err := yaml.Marshal(fgConfig)
	if err != nil {
		return fmt.Errorf("encode Frostgate config: %w", err)
	}
	fgPath := filepath.Join(dir, "config.yaml")
	if err := EnsureConfigDir(dir); err != nil {
		return err
	}
	if err := WriteFileSecure(fgPath, fgContent); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", fgPath)
	return runValidation(dir)
}

func directOnlyFlow(opts Options, dir string) error {
	testURL := config.DefaultTestURL
	custom, err := prompt(opts.Stdin, fmt.Sprintf("Test URL [%s]: ", testURL))
	if err != nil {
		return err
	}
	if custom != "" {
		testURL = custom
	}
	content, err := yaml.Marshal(BuildDirectOnlyConfig(testURL))
	if err != nil {
		return fmt.Errorf("encode Frostgate config: %w", err)
	}
	if err := EnsureConfigDir(dir); err != nil {
		return err
	}
	fgPath := filepath.Join(dir, "config.yaml")
	if err := WriteFileSecure(fgPath, content); err != nil {
		return err
	}
	fmt.Printf("wrote %s (direct only)\n", fgPath)
	return runValidation(dir)
}

func addRouteFlow(opts Options, dir string) error {
	fmt.Println("Add route to existing setup")
	link, err := prompt(opts.Stdin, "Paste new share link (vless:// or hysteria2://): ")
	if err != nil {
		return err
	}
	if link == "" {
		fmt.Println("No link provided")
		return nil
	}
	tag, err := prompt(opts.Stdin, "Name for this route: ")
	if err != nil {
		return err
	}
	if tag == "" {
		return fmt.Errorf("name required")
	}
	if err := routename.Validate(tag); err != nil {
		return fmt.Errorf("invalid route name %q: %w", tag, err)
	}
	if opts.AddRoute == nil {
		return fmt.Errorf("existing setup route management is unavailable")
	}
	if err := opts.AddRoute(dir, link, tag); err != nil {
		return err
	}
	fmt.Printf("added route: %s\n", tag)
	return nil
}

func installSystemdService(dir, binaryPath string, transaction *setupTransaction) error {
	if err := transaction.serviceInstallationAllowed(); err != nil {
		return err
	}
	serviceDir := filepath.Join(filepath.Dir(dir), "systemd", "user")
	servicePath := filepath.Join(serviceDir, "frostgate-singbox.service")
	if unit, err := os.ReadFile(servicePath); err == nil && !unitOwnedByFrostgate(string(unit), filepath.Join(dir, "sing-box.json")) {
		return fmt.Errorf("existing systemd unit does not prove Frostgate ownership")
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect existing Frostgate systemd unit: %w", err)
	}
	if err := InstallSystemdServiceNamed(servicePath, "frostgate-singbox.service", binaryPath, dir, systemdRunnerFactory()); err != nil {
		return err
	}
	if transaction != nil {
		transaction.candidateServiceInstalled = true
	}
	return nil
}

func runValidation(dir string, knownSingBoxPath ...string) error {
	fgPath := filepath.Join(dir, "config.yaml")
	cfg, err := config.Load(fgPath)
	if err != nil {
		fmt.Printf("config check failed: %s\n", redact.Error(err))
		return err
	}
	if !hasProxyEndpoints(cfg) {
		fmt.Printf("config check passed (%d endpoints)\n", len(cfg.Endpoints))
		return nil
	}
	sbPath := ""
	if len(knownSingBoxPath) > 0 {
		sbPath = knownSingBoxPath[0]
	}
	if sbPath == "" {
		sbPath, _ = exec.LookPath("sing-box")
	}
	if sbPath != "" {
		singBoxPath := filepath.Join(dir, "sing-box.json")
		if _, err := os.Stat(singBoxPath); err == nil {
			cmd := exec.Command(sbPath, "check", "-c", singBoxPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Printf("sing-box check failed: %s\n", redact.String(string(out)))
				return err
			}
		}
	}
	fmt.Printf("config check passed (%d endpoints)\n", len(cfg.Endpoints))
	return nil
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

func writeFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".frostgate-install-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	installed := false
	defer func() {
		if installed {
			return
		}
		if removeErr := os.Remove(tmpPath); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, fmt.Errorf("remove temporary file: %w", removeErr))
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	installed = true
	return nil
}
