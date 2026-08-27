package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ilyasmohaimel/frostgate/internal/process"
)

type SystemdRunner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) ([]byte, error)
}

type contextSystemdRunner interface {
	RunContext(ctx context.Context, name string, args ...string) error
	OutputContext(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execSystemdRunner struct{}

func (execSystemdRunner) Run(name string, args ...string) error {
	return process.Run(context.Background(), process.ServiceTimeout, name, args...)
}

func (execSystemdRunner) Output(name string, args ...string) ([]byte, error) {
	return process.Output(context.Background(), process.QuickTimeout, name, args...)
}

func (execSystemdRunner) RunContext(ctx context.Context, name string, args ...string) error {
	return process.Run(ctx, process.ServiceTimeout, name, args...)
}

func (execSystemdRunner) OutputContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	return process.Output(ctx, process.QuickTimeout, name, args...)
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

// BuildSystemdUnitChecked rejects paths which cannot be represented safely in
// the line-oriented unit format before any unit text is generated.
func BuildSystemdUnitChecked(binaryPath, configDir string) (string, error) {
	for _, path := range []string{binaryPath, configDir} {
		if err := validateSystemdPath(path); err != nil {
			return "", err
		}
	}
	return BuildSystemdUnit(binaryPath, configDir), nil
}

func validateSystemdPath(path string) error {
	if path == "" {
		return fmt.Errorf("systemd path is empty")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("systemd path contains unsafe control character")
		}
	}
	return nil
}

func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func InstallSystemdService(servicePath, binaryPath, configDir string, runner SystemdRunner) error {
	return InstallSystemdServiceNamedContext(context.Background(), servicePath, "frostgate-singbox.service", binaryPath, configDir, runner)
}

func ServiceUnitPath(service string) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", service)
}

// WriteSystemdUnit writes a profile-scoped unit without starting it. The
// transaction installs the candidate config before the runner starts it.
func WriteSystemdUnit(servicePath, binaryPath, configDir string) error {
	for _, path := range []string{servicePath, binaryPath, configDir} {
		if err := validateSystemdPath(path); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(servicePath), 0o755); err != nil {
		return fmt.Errorf("create systemd user directory: %w", err)
	}
	if info, err := os.Lstat(servicePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write symlinked systemd unit %s", servicePath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect systemd unit: %w", err)
	}
	unit, err := BuildSystemdUnitChecked(binaryPath, configDir)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(servicePath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	return nil
}

func InstallSystemdServiceNamed(servicePath, service, binaryPath, configDir string, runner SystemdRunner) error {
	return InstallSystemdServiceNamedContext(context.Background(), servicePath, service, binaryPath, configDir, runner)
}

func InstallSystemdServiceNamedContext(ctx context.Context, servicePath, service, binaryPath, configDir string, runner SystemdRunner) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if runner == nil {
		runner = execSystemdRunner{}
	}
	run := func(args ...string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if contextRunner, ok := runner.(contextSystemdRunner); ok {
			return contextRunner.RunContext(ctx, "systemctl", args...)
		}
		return runner.Run("systemctl", args...)
	}
	output := func(args ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if contextRunner, ok := runner.(contextSystemdRunner); ok {
			return contextRunner.OutputContext(ctx, "systemctl", args...)
		}
		return runner.Output("systemctl", args...)
	}
	if err := WriteSystemdUnit(servicePath, binaryPath, configDir); err != nil {
		return err
	}
	if err := run("--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemd daemon-reload: %w", err)
	}
	if err := run("--user", "enable", "--now", service); err != nil {
		return fmt.Errorf("enable %s: %w", service, err)
	}
	// enable --now leaves an already active service running with its old
	// configuration. Restart after daemon-reload so reconfigure actually
	// activates the validated candidate.
	if err := run("--user", "restart", service); err != nil {
		return fmt.Errorf("restart %s: %w", service, err)
	}
	out, err := output("--user", "is-active", service)
	if err != nil {
		return fmt.Errorf("check %s: %w", service, err)
	}
	if strings.TrimSpace(string(out)) != "active" {
		return fmt.Errorf("%s is not active: %s", service, strings.TrimSpace(string(out)))
	}
	return nil
}
