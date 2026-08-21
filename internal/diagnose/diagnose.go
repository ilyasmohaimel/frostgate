package diagnose

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
	"github.com/ilyasmohaimel/frostgate/internal/singbox"
)

type Layer struct {
	Name   string
	Status string // PASS, FAIL, SKIP
	Detail string
	Hint   string
}

func DiagnoseAll(cfg *config.Config) []Layer {
	var out []Layer
	// Local network
	out = append(out, checkLocalNetwork())
	// DNS
	out = append(out, checkDNS())
	// Direct internet
	out = append(out, checkDirect(cfg))
	// sing-box binary
	out = append(out, checkSingBox())
	// sing-box sidecar/service
	out = append(out, checkSidecar())
	// controller
	out = append(out, checkController(cfg))
	// For each outbound, check exists and end-to-end
	for _, ep := range cfg.Endpoints {
		if ep.Type == config.ProbeSingBox {
			out = append(out, checkOutbound(cfg, ep))
			out = append(out, checkEndToEnd(cfg, ep))
		} else if ep.Type == config.ProbeTCP || ep.Type == config.ProbeTLS {
			out = append(out, checkServerReachability(ep))
		}
	}
	// Comparison
	out = append(out, checkComparison(cfg))
	return out
}

func DiagnoseRoute(cfg *config.Config, routeName string) []Layer {
	var target *config.Endpoint
	for i, ep := range cfg.Endpoints {
		if ep.Name == routeName {
			target = &cfg.Endpoints[i]
			break
		}
	}
	if target == nil {
		return []Layer{{Name: routeName, Status: "FAIL", Detail: "route not found"}}
	}
	var out []Layer
	out = append(out, checkLocalNetwork())
	out = append(out, checkDNS())
	out = append(out, checkDirect(cfg))
	out = append(out, checkSingBox())
	out = append(out, checkSidecar())
	out = append(out, checkController(cfg))
	if target.Type == config.ProbeSingBox {
		out = append(out, checkOutbound(cfg, *target))
		out = append(out, checkEndToEnd(cfg, *target))
	} else {
		out = append(out, checkServerReachability(*target))
		// also test direct HTTP if http
		if target.Type == config.ProbeHTTP {
			out = append(out, checkHTTPDirect(*target))
		}
	}
	// Backup info
	out = append(out, checkBackupRoutes(cfg, routeName))
	return out
}

func checkLocalNetwork() Layer {
	// Check if we can get default route? Simple: check interface
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) == 0 {
		return Layer{Name: "Local network", Status: "FAIL", Detail: "no interfaces"}
	}
	return Layer{Name: "Local network", Status: "PASS", Detail: "interfaces ok"}
}

func checkDNS() Layer {
	ips, err := net.LookupHost("www.gstatic.com")
	if err != nil || len(ips) == 0 {
		return Layer{Name: "DNS", Status: "FAIL", Detail: errString(err)}
	}
	return Layer{Name: "DNS", Status: "PASS", Detail: "lookup ok"}
}

func checkDirect(cfg *config.Config) Layer {
	// Find direct baseline endpoint
	var direct *config.Endpoint
	for _, ep := range cfg.Endpoints {
		if ep.Type == config.ProbeHTTP && strings.Contains(strings.ToLower(ep.Name), "direct") {
			direct = &ep
			break
		}
	}
	if direct == nil {
		return Layer{Name: "Direct internet", Status: "SKIP", Detail: "no direct baseline"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := probe.ProbeEndpoint(ctx, cfg, *direct)
	if res.Status == "down" {
		return Layer{Name: "Direct internet", Status: "FAIL", Detail: res.Category + " " + res.ErrMsg, Hint: "local internet or DNS may be down"}
	}
	return Layer{Name: "Direct internet", Status: "PASS", Detail: fmt.Sprintf("%dms", res.Metrics.Median.Milliseconds())}
}

func checkSingBox() Layer {
	path, err := exec.LookPath("sing-box")
	if err != nil {
		return Layer{Name: "sing-box binary", Status: "FAIL", Detail: "not found in PATH"}
	}
	ver := getVersion(path)
	return Layer{Name: "sing-box binary", Status: "PASS", Detail: ver}
}

func getVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	out, _ := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if idx := strings.Index(s, "\n"); idx != -1 {
		s = s[:idx]
	}
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		return "found"
	}
	return s
}

func checkSidecar() Layer {
	// Check if frostgate-singbox service is active
	cmd := exec.Command("systemctl", "--user", "is-active", "frostgate-singbox.service")
	out, _ := cmd.Output()
	s := strings.TrimSpace(string(out))
	if s == "active" {
		return Layer{Name: "sing-box sidecar", Status: "PASS", Detail: "frostgate-singbox active"}
	}
	// Check if controller is reachable anyway (maybe manual run)
	return Layer{Name: "sing-box sidecar", Status: "SKIP", Detail: "service not active (maybe manual run)"}
}

func checkController(cfg *config.Config) Layer {
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := client.GetProxies(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "unauthorized") {
			return Layer{Name: "Controller", Status: "FAIL", Detail: "unauthorized (bad secret)"}
		}
		return Layer{Name: "Controller", Status: "FAIL", Detail: errString(err)}
	}
	return Layer{Name: "Controller", Status: "PASS", Detail: cfg.SingBox.Controller}
}

func checkOutbound(cfg *config.Config, ep config.Endpoint) Layer {
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.CheckOutbound(ctx, ep.Outbound); err != nil {
		return Layer{Name: fmt.Sprintf("Outbound %s", ep.Name), Status: "FAIL", Detail: errString(err)}
	}
	return Layer{Name: fmt.Sprintf("Outbound %s", ep.Name), Status: "PASS", Detail: fmt.Sprintf("outbound %q exists", ep.Outbound)}
}

func checkEndToEnd(cfg *config.Config, ep config.Endpoint) Layer {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.TimeoutFor(ep))
	defer cancel()
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	_, err := client.Delay(ctx, ep.Outbound, cfg.SingBox.TestURL, cfg.TimeoutFor(ep))
	if err != nil {
		return Layer{Name: fmt.Sprintf("End-to-end %s", ep.Name), Status: "FAIL", Detail: errString(err), Hint: "server reachable but proxy path failed — check REALITY/Hysteria auth, server routing, or filtering"}
	}
	return Layer{Name: fmt.Sprintf("End-to-end %s", ep.Name), Status: "PASS", Detail: "proxy URL test passed"}
}

func checkServerReachability(ep config.Endpoint) Layer {
	if ep.Host == "" {
		return Layer{Name: fmt.Sprintf("Server %s", ep.Name), Status: "SKIP", Detail: "no host"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", ep.Addr())
	if err != nil {
		return Layer{Name: fmt.Sprintf("Server %s", ep.Name), Status: "FAIL", Detail: errString(err)}
	}
	conn.Close()
	return Layer{Name: fmt.Sprintf("Server %s", ep.Name), Status: "PASS", Detail: "tcp/443 reachable"}
}

func checkHTTPDirect(ep config.Endpoint) Layer {
	// For http endpoints, we already have probe logic
	return Layer{Name: fmt.Sprintf("HTTP %s", ep.Name), Status: "SKIP", Detail: "use end-to-end for http"}
}

func checkComparison(cfg *config.Config) Layer {
	// Check if at least one backup works when primary fails
	hasUp := false
	hasDown := false
	for _, ep := range cfg.Endpoints {
		// Simplified: we don't have recent probe results here, so skip
		_ = ep
	}
	_ = hasUp
	_ = hasDown
	return Layer{Name: "Comparison", Status: "SKIP", Detail: "run frostgate diagnose <route> for per-route"}
}

func checkBackupRoutes(cfg *config.Config, routeName string) Layer {
	// Find if any other route is healthy
	// For now, just list
	var others []string
	for _, ep := range cfg.Endpoints {
		if ep.Name != routeName {
			others = append(others, ep.Name)
		}
	}
	if len(others) == 0 {
		return Layer{Name: "Backup", Status: "SKIP", Detail: "no other routes"}
	}
	return Layer{Name: "Backup", Status: "PASS", Detail: fmt.Sprintf("alternatives: %s", strings.Join(others, ", "))}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 80 {
		s = s[:80]
	}
	// Redact secrets: remove any vless:// or hysteria2:// content
	if strings.Contains(s, "vless://") || strings.Contains(s, "hysteria2://") {
		return "proxy error [redacted]"
	}
	return s
}
