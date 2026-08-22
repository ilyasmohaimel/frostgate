package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type SystemdRunner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) ([]byte, error)
}

type execSystemdRunner struct{}

func (execSystemdRunner) Run(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func (execSystemdRunner) Output(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// BuildSystemdUnit creates a user unit using the exact paths supplied by the
// caller. Arguments are quoted using systemd's command-line quoting rules.
func BuildSystemdUnit(binaryPath, configDir string) string {
	configPath := filepath.Join(configDir, "sing-box.json")
	return "[Unit]\n" +
		"Description=Frostgate sing-box sidecar\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"ExecStart=" + systemdQuote(binaryPath) + " run -c " + systemdQuote(configPath) + "\n" +
		"Restart=on-failure\n" +
		"RestartSec=5\n" +
		"NoNewPrivileges=true\n" +
		"PrivateTmp=true\n" +
		"ProtectSystem=strict\n" +
		"ProtectHome=read-only\n" +
		"ReadWritePaths=" + systemdQuote(configDir) + "\n\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
}

func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func InstallSystemdService(servicePath, binaryPath, configDir string, runner SystemdRunner) error {
	if runner == nil {
		runner = execSystemdRunner{}
	}
	if err := os.MkdirAll(filepath.Dir(servicePath), 0o755); err != nil {
		return fmt.Errorf("create systemd user directory: %w", err)
	}
	if err := os.WriteFile(servicePath, []byte(BuildSystemdUnit(binaryPath, configDir)), 0o644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	if err := runner.Run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemd daemon-reload: %w", err)
	}
	if err := runner.Run("systemctl", "--user", "enable", "--now", "frostgate-singbox.service"); err != nil {
		return fmt.Errorf("enable frostgate-singbox.service: %w", err)
	}
	out, err := runner.Output("systemctl", "--user", "is-active", "frostgate-singbox.service")
	if err != nil {
		return fmt.Errorf("check frostgate-singbox.service: %w", err)
	}
	if strings.TrimSpace(string(out)) != "active" {
		return fmt.Errorf("frostgate-singbox.service is not active: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
