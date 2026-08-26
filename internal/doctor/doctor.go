package doctor

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/profile"
	"github.com/ilyasmohaimel/frostgate/internal/redact"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
	"github.com/ilyasmohaimel/frostgate/internal/singbox"
)

type Check struct {
	Name   string
	Status string // PASS, FAIL, WARN, SKIP
	Detail string
}

func Run(cfgPath string, verbose bool) []Check {
	resolved := config.ResolveConfigPath(cfgPath)
	return RunForProfile(resolved, filepath.Dir(resolved), verbose)
}

// RunForProfile checks a resolved Frostgate config with its associated managed
// directory. The two differ for the implicit local frostgate.yaml fallback.
func RunForProfile(cfgPath, managedDir string, verbose bool) []Check {
	var out []Check

	// Config path
	resolved := config.ResolveConfigPath(cfgPath)
	if managedDir == "" {
		managedDir = filepath.Dir(resolved)
	}
	out = append(out, Check{Name: "Config path", Status: "PASS", Detail: resolved})
	if _, err := os.Stat(resolved); err != nil {
		out = append(out, Check{Name: "Config file", Status: "FAIL", Detail: redact.Error(err)})
		return out
	}
	out = append(out, Check{Name: "Config file", Status: "PASS", Detail: "exists"})

	out = append(out, privateFileCheck("Config permissions", resolved))
	for _, file := range []struct{ name, path string }{
		{"Controller secret permissions", filepath.Join(managedDir, "controller-secret")},
		{"Managed config permissions", filepath.Join(managedDir, "sing-box.json")},
	} {
		if _, err := os.Stat(file.path); err == nil {
			out = append(out, privateFileCheck(file.name, file.path))
		}
	}

	cfg, err := config.Load(resolved)
	if err != nil {
		out = append(out, Check{Name: "Config parse", Status: "FAIL", Detail: redact.Error(err)})
		return out
	}
	out = append(out, Check{Name: "Config parse", Status: "PASS", Detail: fmt.Sprintf("%d endpoints", len(cfg.Endpoints))})

	// Benchmark proxy configuration (required for proxy throughput)
	hasSBEndpoints := false
	for _, ep := range cfg.Endpoints {
		if ep.Type == config.ProbeSingBox {
			hasSBEndpoints = true
			break
		}
	}
	// singbox.test_url is sidecar-only state. Direct-only configurations may
	// deliberately retain stale sidecar values while remaining healthy.
	if !hasSBEndpoints {
		out = append(out, Check{Name: "Test URL", Status: "SKIP", Detail: "no proxy routes configured"})
	} else if _, err := url.ParseRequestURI(cfg.SingBox.TestURL); err != nil {
		out = append(out, Check{Name: "Test URL", Status: "FAIL", Detail: redact.Error(err)})
	} else {
		out = append(out, Check{Name: "Test URL", Status: "PASS", Detail: redact.String(cfg.SingBox.TestURL)})
	}
	if hasSBEndpoints {
		if cfg.SingBox.BenchmarkProxy == "" {
			if addr, ok := managedBenchmarkProxy(managedDir); ok {
				out = append(out, Check{Name: "Benchmark proxy", Status: "PASS", Detail: fmt.Sprintf("managed listener %s", addr)})
			} else {
				out = append(out, Check{Name: "Benchmark proxy", Status: "WARN", Detail: "singbox.benchmark_proxy not configured; proxy downloads will report N/A"})
			}
		} else if !config.IsLoopbackHost(hostOnly(cfg.SingBox.BenchmarkProxy)) {
			out = append(out, Check{Name: "Benchmark proxy", Status: "FAIL", Detail: fmt.Sprintf("%s is not a loopback address", cfg.SingBox.BenchmarkProxy)})
		} else {
			out = append(out, Check{Name: "Benchmark proxy", Status: "PASS", Detail: cfg.SingBox.BenchmarkProxy})
		}
	}

	// DNS check
	if verbose {
		if err := checkDNS(); err != nil {
			out = append(out, Check{Name: "DNS", Status: "FAIL", Detail: redact.Error(err)})
		} else {
			out = append(out, Check{Name: "DNS", Status: "PASS", Detail: "lookup ok"})
		}
	}

	if !hasSBEndpoints {
		out = append(out,
			Check{Name: "sing-box", Status: "SKIP", Detail: "no proxy routes configured"},
			Check{Name: "Sidecar service", Status: "SKIP", Detail: "no proxy routes configured"},
			Check{Name: "Controller", Status: "SKIP", Detail: "no proxy routes configured"},
		)
		return out
	}

	// sing-box is required only when the selected profile has proxy routes.
	sbPath, err := exec.LookPath("sing-box")
	if err != nil {
		// also check common alt
		if _, err2 := os.Stat("/usr/local/bin/sing-box"); err2 == nil {
			sbPath = "/usr/local/bin/sing-box"
			err = nil
		}
	}
	if err != nil {
		out = append(out, Check{Name: "sing-box", Status: "FAIL", Detail: "not found in PATH; install sing-box for singbox probes"})
	} else {
		ver := getSingBoxVersion(sbPath)
		if ver == "" {
			ver = "found"
		}
		out = append(out, Check{Name: "sing-box", Status: "PASS", Detail: fmt.Sprintf("%s (%s)", ver, sbPath)})
		if hasSBEndpoints {
			u, parseErr := url.Parse(cfg.SingBox.Controller)
			if parseErr != nil || u == nil || u.Hostname() == "" {
				out = append(out, Check{Name: "Controller bind", Status: "FAIL", Detail: "invalid controller address"})
			} else if !config.IsLoopbackHost(u.Hostname()) {
				out = append(out, Check{Name: "Controller bind", Status: "FAIL", Detail: fmt.Sprintf("controller %s is not loopback", redact.String(cfg.SingBox.Controller))})
			} else {
				out = append(out, Check{Name: "Controller bind", Status: "PASS", Detail: redact.String(cfg.SingBox.Controller)})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
			_, cerr := client.GetProxies(ctx)
			cancel()
			if cerr != nil {
				out = append(out, Check{Name: "Controller", Status: "FAIL", Detail: redact.Error(cerr)})
			} else {
				out = append(out, Check{Name: "Controller", Status: "PASS", Detail: redact.String(cfg.SingBox.Controller)})
				// check each outbound
				ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
				proxies, err := client.GetProxies(ctx2)
				cancel2()
				if err == nil {
					for _, ep := range cfg.Endpoints {
						if ep.Type != config.ProbeSingBox {
							continue
						}
						if _, ok := proxies[ep.Outbound]; ok {
							out = append(out, Check{Name: ep.Name, Status: "PASS", Detail: fmt.Sprintf("outbound %q", ep.Outbound)})
						} else {
							out = append(out, Check{Name: ep.Name, Status: "FAIL", Detail: fmt.Sprintf("outbound %q not found", ep.Outbound)})
						}
					}
				}
			}
		}
		defaultDir := setup.ConfigDir(setup.DefaultHomeDir())
		managedCanonical := profile.CanonicalPath(managedDir)
		defaultCanonical := profile.CanonicalPath(defaultDir)
		identity := profile.ForPath(profile.CanonicalPath(resolved), managedCanonical != "" && managedCanonical == defaultCanonical)
		servicePath := setup.ServiceUnitPath(identity.Service)
		if _, err := os.Stat(servicePath); err == nil {
			status := getSystemdStatus(identity.Service)
			if strings.Contains(status, "active (running)") {
				out = append(out, Check{Name: "Sidecar service", Status: "PASS", Detail: strings.TrimSuffix(identity.Service, ".service") + " active"})
			} else if status != "" {
				out = append(out, Check{Name: "Sidecar service", Status: "WARN", Detail: status})
			}
		}
	}

	return out
}

func managedBenchmarkProxy(dir string) (string, bool) {
	return managedconfig.DiscoverBenchmarkProxy(dir)
}

func privateFileCheck(name, path string) Check {
	info, err := os.Stat(path)
	if err != nil {
		return Check{Name: name, Status: "FAIL", Detail: redact.Error(err)}
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return Check{Name: name, Status: "WARN", Detail: fmt.Sprintf("mode %04o allows group/world access; use 600", mode)}
	}
	return Check{Name: name, Status: "PASS", Detail: fmt.Sprintf("%04o", mode)}
}

func checkDNS() error {
	addrs, err := net.LookupHost("www.gstatic.com")
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return fmt.Errorf("no addresses")
	}
	return nil
}

func getSingBoxVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	if i := strings.LastIndex(addr, ":"); i != -1 && !strings.Contains(addr, "]") {
		return addr[:i]
	}
	return addr
}

func getSystemdStatus(unit string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "is-active", unit)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return "systemctl unavailable"
	}
	s := strings.TrimSpace(string(out))
	if s == "active" {
		cmd2 := exec.CommandContext(ctx, "systemctl", "--user", "status", unit, "--no-pager")
		out2, statusErr := cmd2.CombinedOutput()
		if statusErr != nil && len(out2) == 0 {
			return "active (status unavailable)"
		}
		if strings.Contains(string(out2), "active (running)") {
			return "active (running)"
		}
	}
	return s
}

func Format(checks []Check) string {
	var b strings.Builder
	b.WriteString("Frostgate Doctor\n\n")
	for _, c := range checks {
		b.WriteString(fmt.Sprintf("%-20s %s  %s\n", c.Name, c.Status, c.Detail))
	}
	return b.String()
}

func FormatColor(checks []Check) string {
	var b strings.Builder
	b.WriteString("Frostgate Doctor\n\n")
	for _, c := range checks {
		var col string
		switch c.Status {
		case "PASS":
			col = "\033[32m"
		case "FAIL":
			col = "\033[31m"
		case "WARN":
			col = "\033[33m"
		default:
			col = "\033[90m"
		}
		b.WriteString(fmt.Sprintf("%-22s %s%-6s\033[0m %s\n", c.Name, col, c.Status, c.Detail))
	}
	return b.String()
}
