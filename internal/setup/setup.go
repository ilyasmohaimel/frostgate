package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
)

type Options struct {
	HomeDir string
	Stdin   *bufio.Reader
	Stdout  *os.File
	Stderr  *os.File
}

func DefaultHomeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	hd, _ := os.UserHomeDir()
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
	// Use os.Stdout/ Stderr if not set
	out := os.Stdout
	if opts.Stdout != nil {
		out = opts.Stdout
	}
	_ = out

	dir := ConfigDir(opts.HomeDir)
	fmt.Printf("\nFrostgate setup\n\n")
	fmt.Printf("Config directory: %s\n", dir)
	fmt.Printf("OS: %s  Arch: %s\n\n", runtime.GOOS, runtime.GOARCH)

	// Preflight: existing config?
	existingConfig := filepath.Join(dir, "config.yaml")
	existingSingBox := filepath.Join(dir, "sing-box.json")
	existingSecret := filepath.Join(dir, "controller-secret")
	hasExisting := false
	if _, err := os.Stat(existingConfig); err == nil {
		hasExisting = true
	}
	if hasExisting {
		fmt.Printf("Existing Frostgate config found at %s\n", existingConfig)
		fmt.Printf("[a] Add route  [r] Reconfigure  [x] Reset  [c] Cancel\n")
		choice := prompt(opts.Stdin, "Choose [a/r/x/c] (default c): ")
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "a":
			return addRouteFlow(opts, dir)
		case "r":
			// fall through to full setup but will backup
			fmt.Println("Reconfiguring; existing config will be backed up")
			backupExisting(dir)
		case "x":
			confirm := prompt(opts.Stdin, "Type YES to reset and delete existing config: ")
			if confirm != "YES" {
				fmt.Println("Cancelled")
				return nil
			}
			backupExisting(dir)
			// remove
			os.Remove(existingConfig)
			os.Remove(existingSingBox)
			// keep secret? regenerate? For reset, remove secret too to regenerate
			os.Remove(existingSecret)
			fmt.Println("Existing config removed")
		default:
			fmt.Println("Cancelled; no changes made")
			return nil
		}
	}

	// Ensure dir with 0700
	if err := EnsureConfigDir(dir); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	fmt.Printf("config directory ready: %s (0700)\n", dir)

	// sing-box detection
	sbPath, err := exec.LookPath("sing-box")
	if err != nil {
		fmt.Println("\nsing-box not found in PATH")
		fmt.Println("Frostgate uses sing-box as the protocol engine (no proxy protocols are reimplemented in Frostgate).")
		install := prompt(opts.Stdin, "Install sing-box now? [Y/n]: ")
		if !isNo(install) {
			if err := tryInstallSingBox(opts); err != nil {
				fmt.Printf("Automatic install failed: %v\n", err)
				fmt.Println("Please install sing-box manually: https://sing-box.sagernet.org/installation/")
				return err
			}
			sbPath, _ = exec.LookPath("sing-box")
		} else {
			fmt.Println("Continuing without sing-box. Only direct checks will work until it is installed.")
		}
	}
	if sbPath != "" {
		ver := getSingBoxVersion(sbPath)
		fmt.Printf("sing-box found: %s (%s)\n", ver, sbPath)
	}

	// Choose setup type
	fmt.Println("\nHow do you want to configure Frostgate?")
	fmt.Println("  1) Import proxy route(s) (VLESS/Hysteria2 share links)")
	fmt.Println("  2) Use existing sing-box controller (advanced)")
	fmt.Println("  3) Direct internet checks only (no proxy)")
	choice := prompt(opts.Stdin, "Choose [1/2/3] (default 1): ")
	if choice == "" {
		choice = "1"
	}
	switch choice {
	case "1":
		return importFlow(opts, dir)
	case "2":
		return existingControllerFlow(opts, dir)
	case "3":
		return directOnlyFlow(opts, dir)
	default:
		fmt.Println("Invalid choice, defaulting to import")
		return importFlow(opts, dir)
	}
}

func prompt(r *bufio.Reader, msg string) string {
	fmt.Print(msg)
	s, _ := r.ReadString('\n')
	return strings.TrimSpace(s)
}

func isNo(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "n" || s == "no"
}

func backupExisting(dir string) {
	ts := time.Now().Format("20060102T150405")
	backupDir := filepath.Join(dir, "backup-"+ts)
	os.MkdirAll(backupDir, 0o700)
	for _, name := range []string{"config.yaml", "sing-box.json", "controller-secret"} {
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); err == nil {
			data, _ := os.ReadFile(src)
			os.WriteFile(filepath.Join(backupDir, name), data, 0o600)
		}
	}
	fmt.Printf("Backup saved to %s\n", backupDir)
}

func tryInstallSingBox(opts Options) error {
	switch runtime.GOOS {
	case "linux":
		// Prefer pacman on Arch, otherwise advise
		if _, err := exec.LookPath("pacman"); err == nil {
			fmt.Println("Attempting: sudo pacman -S --noconfirm sing-box")
			cmd := exec.Command("sudo", "pacman", "-S", "--noconfirm", "sing-box")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
			return nil
		}
		fmt.Println("No pacman found. Downloading static binary from GitHub...")
		// fallback: download from GitHub releases
		// Use latest version 1.13.19 as known stable
		url := "https://github.com/SagerNet/sing-box/releases/download/v1.13.19/sing-box-1.13.19-linux-amd64.tar.gz"
		tmpDir, _ := os.MkdirTemp("", "sing-box-install")
		defer os.RemoveAll(tmpDir)
		fmt.Printf("Downloading %s ...\n", url)
		// try curl/wget
		var cmd *exec.Cmd
		if _, err := exec.LookPath("curl"); err == nil {
			cmd = exec.Command("curl", "-fL", url, "-o", filepath.Join(tmpDir, "sing-box.tar.gz"))
		} else if _, err := exec.LookPath("wget"); err == nil {
			cmd = exec.Command("wget", "-qO", filepath.Join(tmpDir, "sing-box.tar.gz"), url)
		} else {
			return fmt.Errorf("neither curl nor wget found")
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		// extract
		ex := exec.Command("tar", "xzf", filepath.Join(tmpDir, "sing-box.tar.gz"), "-C", tmpDir)
		if err := ex.Run(); err != nil {
			return err
		}
		// find binary
		bin := filepath.Join(tmpDir, "sing-box-1.13.19-linux-amd64", "sing-box")
		if _, err := os.Stat(bin); err != nil {
			// try find
			out, _ := exec.Command("find", tmpDir, "-name", "sing-box", "-type", "f").Output()
			bin = strings.TrimSpace(string(out))
			if bin == "" {
				return fmt.Errorf("sing-box binary not found in archive")
			}
		}
		installDir := filepath.Join(opts.HomeDir, ".local", "bin")
		os.MkdirAll(installDir, 0o755)
		dest := filepath.Join(installDir, "sing-box")
		data, _ := os.ReadFile(bin)
		os.WriteFile(dest, data, 0o755)
		fmt.Printf("Installed sing-box to %s\n", dest)
		// check PATH
		if !isInPath(installDir) {
			fmt.Printf("NOTE: %s not in PATH. Add: export PATH=\"$HOME/.local/bin:$PATH\"\n", installDir)
		}
		return nil
	default:
		return fmt.Errorf("automatic install not supported on %s; see https://sing-box.sagernet.org/installation/", runtime.GOOS)
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
	out, _ := cmd.CombinedOutput()
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

func importFlow(opts Options, dir string) error {
	var vlessOut []*VLESSOutbound
	var hystOut []*Hysteria2Outbound

	fmt.Println("\nImport proxy routes")
	fmt.Println("Paste your share links (vless:// or hysteria2:// / hy2://)")
	fmt.Println("Leave empty to finish. Your input is not logged.")
	for {
		link := prompt(opts.Stdin, "\nPaste share link (empty to finish): ")
		if link == "" {
			if len(vlessOut)+len(hystOut) == 0 {
				// allow direct only fallback
				fmt.Println("No routes added yet.")
				cont := prompt(opts.Stdin, "Add direct baseline instead? [Y/n]: ")
				if !isNo(cont) {
					break
				}
				continue
			}
			break
		}
		tag := prompt(opts.Stdin, "Name for this route (e.g. reality-primary): ")
		if tag == "" {
			fmt.Println("Name required")
			continue
		}
		// sanitize tag
		tag = strings.TrimSpace(tag)
		if strings.Contains(tag, " ") {
			tag = strings.ReplaceAll(tag, " ", "-")
		}
		linkLower := strings.ToLower(strings.TrimSpace(link))
		if strings.HasPrefix(linkLower, "vless://") {
			v, err := ParseVLESSShareLink(link, tag)
			if err != nil {
				fmt.Printf("Failed to parse VLESS: %v (redacted: %s)\n", err, RedactURL(link))
				continue
			}
			vlessOut = append(vlessOut, v)
			fmt.Printf("added VLESS: %s\n", tag)
		} else if strings.HasPrefix(linkLower, "hysteria2://") || strings.HasPrefix(linkLower, "hy2://") {
			h, err := ParseHysteria2ShareLink(link, tag)
			if err != nil {
				fmt.Printf("Failed to parse Hysteria2: %v (redacted: %s)\n", err, RedactURL(link))
				continue
			}
			hystOut = append(hystOut, h)
			fmt.Printf("added Hysteria2: %s\n", tag)
		} else {
			fmt.Printf("Unsupported link (must be vless:// or hysteria2://)\n")
			continue
		}
		more := prompt(opts.Stdin, "Add another route? [y/N]: ")
		if !strings.EqualFold(more, "y") && !strings.EqualFold(more, "yes") {
			// continue to allow empty finish? Actually break if they said N and have at least one
			if more == "" {
				// treat empty as no more if they already added at least one
				break
			}
			if isNo(more) {
				break
			}
		}
	}

	// Direct baseline offer
	testURL := "https://www.gstatic.com/generate_204"
	useDirect := prompt(opts.Stdin, fmt.Sprintf("Add direct baseline (%s)? [Y/n]: ", testURL))
	addDirect := true
	if isNo(useDirect) {
		custom := prompt(opts.Stdin, "Custom test URL (empty to skip direct): ")
		if custom != "" {
			testURL = custom
			addDirect = true
		} else {
			addDirect = false
		}
	}

	// Controller secret
	secretFile := filepath.Join(dir, "controller-secret")
	var secret string
	if data, err := os.ReadFile(secretFile); err == nil {
		secret = strings.TrimSpace(string(data))
		fmt.Println("using existing controller secret")
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
	singBoxPath := filepath.Join(dir, "sing-box.json")
	if err := WriteFileSecure(singBoxPath, singBoxData); err != nil {
		return err
	}
	fmt.Printf("wrote %s (0600)\n", singBoxPath)

	// Validate sing-box
	if sbPath, err := exec.LookPath("sing-box"); err == nil {
		cmd := exec.Command(sbPath, "check", "-c", singBoxPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("sing-box check failed: %s\n", string(out))
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
	fgContent := GenerateFrostgateConfig(vlessTags, hystTags, testURL, controller, secretFileForConfig)
	// If addDirect false, remove direct endpoint from generated content? GenerateFrostgateConfig always adds direct, so we need to handle
	if !addDirect {
		// remove direct block
		// simple: replace direct endpoint lines
		fgContent = strings.Replace(fgContent, "  - name: direct\n    type: http\n    url: https://www.gstatic.com/generate_204\n    priority: 100\n", "", 1)
		if !strings.Contains(fgContent, "endpoints:") || strings.TrimSpace(fgContent) == "" {
			// ensure at least one endpoint, add direct back if no other
			if len(vlessTags)+len(hystTags) == 0 {
				fgContent += "  - name: direct\n    type: http\n    url: https://www.gstatic.com/generate_204\n    priority: 100\n"
			}
		}
	}
	fgPath := filepath.Join(dir, "config.yaml")
	if err := WriteFileSecure(fgPath, []byte(fgContent)); err != nil {
		return err
	}
	fmt.Printf("wrote %s (0600)\n", fgPath)

	// Sidecar service
	if runtime.GOOS == "linux" {
		offer := prompt(opts.Stdin, "Install systemd user service for sing-box sidecar? [Y/n]: ")
		if !isNo(offer) {
			if err := installSystemdService(dir); err != nil {
				fmt.Printf("Failed to install service: %v\n", err)
			} else {
				fmt.Println("systemd service installed and started")
			}
		}
	}

	// Validate
	fmt.Println("\nValidating...")
	if err := runValidation(dir); err != nil {
		fmt.Printf("Validation failed: %v\n", err)
		return err
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
	fmt.Println("\nsing-box: run `sing-box run -c ~/.config/frostgate/sing-box.json` if no service was installed")
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
	controller := prompt(opts.Stdin, "Controller URL [http://127.0.0.1:9090]: ")
	if controller == "" {
		controller = "http://127.0.0.1:9090"
	}
	secret := prompt(opts.Stdin, "Controller secret (leave empty if none): ")
	secretFile := ""
	if secret != "" {
		// offer to save to file
		save := prompt(opts.Stdin, "Save secret to controller-secret file? [Y/n]: ")
		if !isNo(save) {
			secretFile = filepath.Join(dir, "controller-secret")
			WriteSecretFile(secretFile, secret)
			secret = "" // use file
		}
	}
	// Ask for outbound tags
	fmt.Println("Enter your existing sing-box outbound tags (comma or space separated)")
	tagsStr := prompt(opts.Stdin, "Tags (e.g. reality,hysteria): ")
	var tags []string
	for _, t := range strings.FieldsFunc(tagsStr, func(r rune) bool { return r == ',' || r == ' ' }) {
		if t != "" {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return fmt.Errorf("no tags provided")
	}
	testURL := "https://www.gstatic.com/generate_204"
	// generate Frostgate config only (no sing-box.json)
	if secretFile != "" {
		secret = "" // will use file
	}
	fgContent := GenerateFrostgateConfig(nil, nil, testURL, controller, secretFile)
	// Actually need to add endpoints for each tag
	// Replace generated direct + tags appropriately
	// For existing controller, we don't know which are vless vs hysteria, just create generic
	fgContent = "defaults:\n  timeout: 5s\n  samples: 4\n  sample_interval: 150ms\n  healthy_latency: 800ms\n  max_jitter: 80ms\n\nsingbox:\n  controller: " + controller + "\n"
	if secretFile != "" {
		fgContent += "  secret_file: " + secretFile + "\n"
	} else if secret != "" {
		fgContent += "  secret: " + secret + "\n"
	}
	fgContent += "  test_url: " + testURL + "\n\nendpoints:\n"
	fgContent += "  - name: direct\n    type: http\n    url: " + testURL + "\n    priority: 100\n"
	for i, tag := range tags {
		fgContent += fmt.Sprintf("  - name: %s\n    type: singbox\n    outbound: %s\n    priority: %d\n", tag, tag, 10+i*10)
	}
	fgPath := filepath.Join(dir, "config.yaml")
	EnsureConfigDir(dir)
	WriteFileSecure(fgPath, []byte(fgContent))
	fmt.Printf("wrote %s\n", fgPath)
	return runValidation(dir)
}

func directOnlyFlow(opts Options, dir string) error {
	testURL := "https://www.gstatic.com/generate_204"
	custom := prompt(opts.Stdin, fmt.Sprintf("Test URL [%s]: ", testURL))
	if custom != "" {
		testURL = custom
	}
	content := fmt.Sprintf("defaults:\n  timeout: 5s\n  samples: 4\n  sample_interval: 150ms\n  healthy_latency: 800ms\n  max_jitter: 80ms\n\nsingbox:\n  controller: http://127.0.0.1:9090\n  test_url: %s\n\nendpoints:\n  - name: direct\n    type: http\n    url: %s\n    priority: 100\n", testURL, testURL)
	EnsureConfigDir(dir)
	fgPath := filepath.Join(dir, "config.yaml")
	WriteFileSecure(fgPath, []byte(content))
	fmt.Printf("wrote %s (direct only)\n", fgPath)
	return runValidation(dir)
}

func addRouteFlow(opts Options, dir string) error {
	// Load existing sing-box.json and config.yaml, append new route
	fmt.Println("Add route to existing setup")
	// For simplicity, run importFlow but preserve existing
	// Backup already done in caller
	// We will just run importFlow which will prompt for new routes and regenerate with existing+new
	// To preserve existing, we need to read existing sing-box.json
	// Simpler: just run importFlow fresh and it will generate new config with new routes + direct
	// But we risk losing existing routes if user only adds one new
	// For now, run importFlow and it will add new routes to existing dir (overwrites but includes new)
	// We could load existing outbounds and merge
	singBoxPath := filepath.Join(dir, "sing-box.json")
	var existingVless []*VLESSOutbound
	var existingHyst []*Hysteria2Outbound
	if data, err := os.ReadFile(singBoxPath); err == nil {
		var cfg map[string]any
		if jsonErr := json.Unmarshal(data, &cfg); jsonErr == nil {
			if outbounds, ok := cfg["outbounds"].([]any); ok {
				for _, ob := range outbounds {
					if m, ok := ob.(map[string]any); ok {
						if t, _ := m["type"].(string); t == "vless" {
							// reconstruct minimal
							tag, _ := m["tag"].(string)
							server, _ := m["server"].(string)
							portF, _ := m["server_port"].(float64)
							uuid, _ := m["uuid"].(string)
							// try to extract tls
							sni := ""
							pbk := ""
							sid := ""
							fp := "chrome"
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
							existingVless = append(existingVless, &VLESSOutbound{Tag: tag, Server: server, ServerPort: int(portF), UUID: uuid, SNI: sni, PBK: pbk, SID: sid, FP: fp})
						} else if t == "hysteria2" {
							tag, _ := m["tag"].(string)
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
							existingHyst = append(existingHyst, &Hysteria2Outbound{Tag: tag, Server: server, ServerPort: int(portF), Password: pass, ObfsPassword: obfsPass, SNI: sni})
						}
					}
				}
			}
		}
	}
	// Now run import for new route, but we will merge existing after
	var vlessOut []*VLESSOutbound
	var hystOut []*Hysteria2Outbound
	vlessOut = append(vlessOut, existingVless...)
	hystOut = append(hystOut, existingHyst...)
	// Prompt for new link
	link := prompt(opts.Stdin, "Paste new share link (vless:// or hysteria2://): ")
	if link == "" {
		fmt.Println("No link provided")
		return nil
	}
	tag := prompt(opts.Stdin, "Name for this route: ")
	if tag == "" {
		return fmt.Errorf("name required")
	}
	linkLower := strings.ToLower(strings.TrimSpace(link))
	if strings.HasPrefix(linkLower, "vless://") {
		v, err := ParseVLESSShareLink(link, tag)
		if err != nil {
			return err
		}
		vlessOut = append(vlessOut, v)
	} else if strings.HasPrefix(linkLower, "hysteria2://") || strings.HasPrefix(linkLower, "hy2://") {
		h, err := ParseHysteria2ShareLink(link, tag)
		if err != nil {
			return err
		}
		hystOut = append(hystOut, h)
	} else {
		return fmt.Errorf("unsupported link")
	}
	// Regenerate
	secretFile := filepath.Join(dir, "controller-secret")
	secret := ""
	if data, err := os.ReadFile(secretFile); err == nil {
		secret = strings.TrimSpace(string(data))
	} else {
		s, _ := GenerateSecret()
		secret = s
		WriteSecretFile(secretFile, secret)
	}
	singBoxData, err := GenerateSingBoxConfig(secret, vlessOut, hystOut, dir)
	if err != nil {
		return err
	}
	if err := WriteFileSecure(singBoxPath, singBoxData); err != nil {
		return err
	}
	// Regenerate Frostgate config
	var vlessTags []string
	for _, v := range vlessOut {
		vlessTags = append(vlessTags, v.Tag)
	}
	var hystTags []string
	for _, h := range hystOut {
		hystTags = append(hystTags, h.Tag)
	}
	fgContent := GenerateFrostgateConfig(vlessTags, hystTags, "https://www.gstatic.com/generate_204", "http://127.0.0.1:9090", secretFile)
	fgPath := filepath.Join(dir, "config.yaml")
	WriteFileSecure(fgPath, []byte(fgContent))
	fmt.Printf("added route: %s\n", tag)
	return runValidation(dir)
}

func installSystemdService(dir string) error {
	// systemd user dir is $XDG_CONFIG_HOME/systemd/user or ~/.config/systemd/user
	// dir is like $XDG_CONFIG_HOME/frostgate or ~/.config/frostgate, so parent is config home
	serviceDir := filepath.Join(filepath.Dir(dir), "systemd", "user")
	if _, err := os.Stat(serviceDir); err != nil {
		// fallback to ~/.config/systemd/user
		home, _ := os.UserHomeDir()
		serviceDir = filepath.Join(home, ".config", "systemd", "user")
	}
	os.MkdirAll(serviceDir, 0o755)
	servicePath := filepath.Join(serviceDir, "frostgate-singbox.service")
	content := `[Unit]
Description=Frostgate sing-box sidecar (Clash API for end-to-end route tests)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/sing-box run -c %h/.config/frostgate/sing-box.json
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/.config/frostgate

[Install]
WantedBy=default.target
`
	if err := os.WriteFile(servicePath, []byte(content), 0o644); err != nil {
		return err
	}
	// daemon-reload and enable
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	exec.Command("systemctl", "--user", "enable", "--now", "frostgate-singbox.service").Run()
	// validate started
	time.Sleep(1 * time.Second)
	out, _ := exec.Command("systemctl", "--user", "is-active", "frostgate-singbox.service").Output()
	if strings.TrimSpace(string(out)) != "active" {
		return fmt.Errorf("service not active: %s", string(out))
	}
	return nil
}

func runValidation(dir string) error {
	// sing-box check
	sbPath, err := exec.LookPath("sing-box")
	if err == nil {
		singBoxPath := filepath.Join(dir, "sing-box.json")
		if _, err := os.Stat(singBoxPath); err == nil {
			cmd := exec.Command(sbPath, "check", "-c", singBoxPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Printf("sing-box check failed: %s\n", string(out))
				return err
			}
		}
	}
	// frostgate --check
	fgPath := filepath.Join(dir, "config.yaml")
	cfg, err := config.Load(fgPath)
	if err != nil {
		fmt.Printf("config check failed: %v\n", err)
		return err
	}
	fmt.Printf("config check passed (%d endpoints)\n", len(cfg.Endpoints))
	// frostgate --doctor via internal doctor
	// Use doctor.Run
	// Import doctor
	// To avoid import cycle, we will just call frostgate binary if available
	if _, err := exec.LookPath("frostgate"); err == nil {
		cmd := exec.Command("frostgate", "--check", "--config", fgPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("frostgate --check failed: %s\n", string(out))
		} else {
			fmt.Printf("frostgate check passed\n")
		}
	}
	return nil
}
