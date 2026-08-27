package diagnose

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
	"github.com/ilyasmohaimel/frostgate/internal/process"
	"github.com/ilyasmohaimel/frostgate/internal/redact"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
	"github.com/ilyasmohaimel/frostgate/internal/singbox"
)

type Layer struct {
	Name   string `json:"name"`
	Status string `json:"status"` // PASS, FAIL, SKIP, WARN
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// Options carries optional context for diagnosis. Managed metadata lets the
// engine inspect the real server fields (host/port/SNI) without ever printing
// secrets.
type Options struct {
	Managed        *managedconfig.Managed
	SidecarService string
}

var (
	diagnoseLookupTimeout  = 5 * time.Second
	diagnoseLookupHost     = net.DefaultResolver.LookupHost
	diagnoseCommandTimeout = process.QuickTimeout
)

func DiagnoseAll(cfg *config.Config, opts Options) []Layer {
	var out []Layer
	out = append(out, checkLocalNetwork())
	out = append(out, checkDNS(cfg))
	out = append(out, checkDirect(cfg))
	if hasSingBoxEndpoints(cfg) {
		out = append(out, localOwnershipLayers(opts)...)
		out = append(out, checkController(cfg))
	} else {
		out = append(out,
			Layer{Name: "sing-box binary", Status: "SKIP", Detail: "no proxy routes configured"},
			Layer{Name: "sing-box sidecar", Status: "SKIP", Detail: "no proxy routes configured"},
			Layer{Name: "Controller", Status: "SKIP", Detail: "no proxy routes configured"},
		)
	}
	for _, ep := range cfg.Endpoints {
		switch ep.Type {
		case config.ProbeSingBox:
			out = append(out, diagnoseProxyRoute(cfg, ep, opts)...)
		case config.ProbeTCP, config.ProbeTLS:
			out = append(out, checkServerReachability(ep))
		}
	}
	return out
}

func DiagnoseRoute(cfg *config.Config, routeName string, opts Options) []Layer {
	var target *config.Endpoint
	for i := range cfg.Endpoints {
		if cfg.Endpoints[i].Name == routeName {
			target = &cfg.Endpoints[i]
			break
		}
	}
	if target == nil {
		return []Layer{{Name: routeName, Status: "FAIL", Detail: "route not found"}}
	}
	var out []Layer
	out = append(out, checkLocalNetwork())
	out = append(out, checkDNS(cfg))
	out = append(out, checkDirect(cfg))
	if hasSingBoxEndpoints(cfg) {
		out = append(out, localOwnershipLayers(opts)...)
		out = append(out, checkController(cfg))
	} else {
		out = append(out,
			Layer{Name: "sing-box binary", Status: "SKIP", Detail: "no proxy routes configured"},
			Layer{Name: "sing-box sidecar", Status: "SKIP", Detail: "no proxy routes configured"},
			Layer{Name: "Controller", Status: "SKIP", Detail: "no proxy routes configured"},
		)
	}
	if target.Type == config.ProbeSingBox {
		out = append(out, diagnoseProxyRoute(cfg, *target, opts)...)
	} else {
		out = append(out, checkServerReachability(*target))
	}
	out = append(out, checkBackupRoutes(cfg, routeName)...)
	return out
}

func hasSingBoxEndpoints(cfg *config.Config) bool {
	for _, ep := range cfg.Endpoints {
		if ep.Type == config.ProbeSingBox {
			return true
		}
	}
	return false
}

func localOwnershipLayers(opts Options) []Layer {
	if opts.Managed != nil && opts.Managed.HasManagedSidecar() {
		return []Layer{checkSingBox(), checkSidecar(opts.SidecarService)}
	}
	return []Layer{
		{Name: "sing-box binary", Status: "SKIP", Detail: "profile does not prove a Frostgate-managed sidecar"},
		{Name: "sing-box sidecar", Status: "SKIP", Detail: "external or manual sidecar ownership"},
	}
}

// diagnoseProxyRoute emits evidence-based layers for a singbox-backed route.
// VLESS+REALITY RAW/TCP routes get measured DNS and TCP layers; Hysteria2 gets
// honest wording because UDP reachability cannot be proven with a generic
// dial. Secrets from managed metadata are never included in details.
func diagnoseProxyRoute(cfg *config.Config, ep config.Endpoint, opts Options) []Layer {
	var out []Layer
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	switch {
	case vlessMeta(opts, ep.Outbound) != nil:
		v := vlessMeta(opts, ep.Outbound)
		out = append(out, checkDNSForHost(ep.Name, v.Server))
		out = append(out, checkTCPServer(ep.Name, v.Server, v.ServerPort))
		out = append(out, checkOutbound(client, ep.Name, ep.Outbound))
		out = append(out, checkEndToEnd(client, cfg, ep, "vless"))
	case hysteriaMeta(opts, ep.Outbound) != nil:
		out = append(out, Layer{
			Name:   fmt.Sprintf("Server %s", ep.Name),
			Status: "SKIP",
			Detail: "Low-level server reachability: not independently verified (UDP/QUIC)",
		})
		out = append(out, checkOutbound(client, ep.Name, ep.Outbound))
		out = append(out, checkEndToEnd(client, cfg, ep, "hysteria2"))
	default:
		out = append(out, checkOutbound(client, ep.Name, ep.Outbound))
		out = append(out, checkEndToEnd(client, cfg, ep, "generic"))
	}
	return out
}

func vlessMeta(opts Options, tag string) *setup.VLESSOutbound {
	if opts.Managed == nil {
		return nil
	}
	for _, v := range opts.Managed.VLESS {
		if v != nil && v.Tag == tag {
			return v
		}
	}
	return nil
}

func hysteriaMeta(opts Options, tag string) *setup.Hysteria2Outbound {
	if opts.Managed == nil {
		return nil
	}
	for _, h := range opts.Managed.Hysteria {
		if h != nil && h.Tag == tag {
			return h
		}
	}
	return nil
}

func checkLocalNetwork() Layer {
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) == 0 {
		return Layer{Name: "Local network", Status: "FAIL", Detail: "no interfaces"}
	}
	return Layer{Name: "Local network", Status: "PASS", Detail: "interfaces ok"}
}

func checkDNS(cfg *config.Config) Layer {
	host := "www.gstatic.com"
	for _, ep := range cfg.Endpoints {
		if ep.Type != config.ProbeHTTP {
			continue
		}
		if u, err := url.Parse(ep.EffectiveURL()); err == nil && u.Hostname() != "" {
			host = u.Hostname()
			break
		}
	}
	if net.ParseIP(host) != nil {
		return Layer{Name: "DNS", Status: "PASS", Detail: "not required for IP literal"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), diagnoseLookupTimeout)
	defer cancel()
	ips, err := diagnoseLookupHost(ctx, host)
	if err != nil || len(ips) == 0 {
		return Layer{Name: "DNS", Status: "FAIL", Detail: errString(err)}
	}
	return Layer{Name: "DNS", Status: "PASS", Detail: "lookup ok"}
}

func checkDNSForHost(routeName, host string) Layer {
	if host == "" {
		return Layer{Name: fmt.Sprintf("DNS %s", routeName), Status: "SKIP", Detail: "no server host"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), diagnoseLookupTimeout)
	defer cancel()
	addrs, err := diagnoseLookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		return Layer{
			Name:   fmt.Sprintf("DNS %s", routeName),
			Status: "FAIL",
			Detail: fmt.Sprintf("cannot resolve %q", host),
			Hint:   "check DNS or server hostname",
		}
	}
	return Layer{
		Name:   fmt.Sprintf("DNS %s", routeName),
		Status: "PASS",
		Detail: fmt.Sprintf("resolved %d address(es)", len(addrs)),
	}
}

// checkTCPServer measures an actual TCP connection to server:port. The
// "reachable" claim is only made after a real successful dial.
func checkTCPServer(routeName, host string, port int) Layer {
	name := fmt.Sprintf("Server TCP %s", routeName)
	if host == "" || port == 0 {
		return Layer{Name: name, Status: "SKIP", Detail: "no server address"}
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	start := time.Now()
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return Layer{
			Name:   name,
			Status: "FAIL",
			Detail: fmt.Sprintf("tcp/%d connect failed", port),
			Hint:   "server down, wrong port, or firewall",
		}
	}
	if err := conn.Close(); err != nil {
		return Layer{Name: name, Status: "FAIL", Detail: "tcp connection close failed", Hint: "local socket error"}
	}
	ms := time.Since(start).Milliseconds()
	return Layer{
		Name:   name,
		Status: "PASS",
		Detail: fmt.Sprintf("tcp/%d reachable (%dms)", port, ms),
	}
}

func checkDirect(cfg *config.Config) Layer {
	var direct *config.Endpoint
	for i := range cfg.Endpoints {
		ep := cfg.Endpoints[i]
		if ep.Type == config.ProbeHTTP && probe.IsDirectBaseline(ep) {
			direct = &ep
			break
		}
	}
	if direct == nil {
		return Layer{Name: "Direct internet", Status: "SKIP", Detail: "no direct baseline"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ProbeBudget(*direct))
	defer cancel()
	res := probe.ProbeEndpoint(ctx, cfg, *direct)
	if res.Status == "down" {
		return Layer{Name: "Direct internet", Status: "FAIL", Detail: strings.TrimSpace(res.Category + " " + redact.String(res.ErrMsg)), Hint: "local internet or DNS may be down"}
	}
	return Layer{Name: "Direct internet", Status: "PASS", Detail: fmt.Sprintf("%dms", res.Metrics.Median.Milliseconds())}
}

func checkSingBox() Layer {
	path, err := exec.LookPath("sing-box")
	if err != nil {
		return Layer{Name: "sing-box binary", Status: "FAIL", Detail: "not found in PATH"}
	}
	return Layer{Name: "sing-box binary", Status: "PASS", Detail: getVersion(path)}
}

func getVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), process.QuickTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil && s == "" {
		return "found (version unavailable)"
	}
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

func checkSidecar(service string) Layer {
	if service == "" {
		service = "frostgate-singbox.service"
	}
	out, err := process.Output(context.Background(), diagnoseCommandTimeout, "systemctl", "--user", "is-active", service)
	s := strings.TrimSpace(string(out))
	if s == "active" {
		return Layer{Name: "sing-box sidecar", Status: "PASS", Detail: strings.TrimSuffix(service, ".service") + " active"}
	}
	if err != nil && s == "" {
		return Layer{Name: "sing-box sidecar", Status: "SKIP", Detail: "systemd user service unavailable or inactive (maybe manual run)"}
	}
	return Layer{Name: "sing-box sidecar", Status: "SKIP", Detail: "service not active (maybe manual run)"}
}

func checkController(cfg *config.Config) Layer {
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	ctx, cancel := context.WithTimeout(context.Background(), process.QuickTimeout)
	defer cancel()
	if _, err := client.GetProxies(ctx); err != nil {
		if strings.Contains(err.Error(), "unauthorized") {
			return Layer{Name: "Controller", Status: "FAIL", Detail: "unauthorized (bad secret)"}
		}
		return Layer{Name: "Controller", Status: "FAIL", Detail: errString(err)}
	}
	return Layer{Name: "Controller", Status: "PASS", Detail: redact.String(cfg.SingBox.Controller)}
}

func checkOutbound(client *singbox.Client, routeName, outbound string) Layer {
	ctx, cancel := context.WithTimeout(context.Background(), process.QuickTimeout)
	defer cancel()
	if err := client.CheckOutbound(ctx, outbound); err != nil {
		return Layer{Name: fmt.Sprintf("Outbound %s", routeName), Status: "FAIL", Detail: errString(err)}
	}
	return Layer{Name: fmt.Sprintf("Outbound %s", routeName), Status: "PASS", Detail: fmt.Sprintf("outbound %q exists", outbound)}
}

func checkEndToEnd(client *singbox.Client, cfg *config.Config, ep config.Endpoint, kind string) Layer {
	timeout := cfg.TimeoutFor(ep)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	d, err := client.Delay(ctx, ep.Outbound, cfg.SingBox.TestURL, timeout)
	if err != nil {
		hint := ""
		switch kind {
		case "vless":
			hint = "check UUID, REALITY public key, short ID, SNI, server, or filtering"
		case "hysteria2":
			hint = "check Hysteria2 password, obfs, SNI, UDP reachability, or server availability"
		default:
			hint = "check the controller route configuration and upstream connectivity"
		}
		return Layer{
			Name:   endToEndName(kind, ep),
			Status: "FAIL",
			Detail: errString(err),
			Hint:   hint,
		}
	}
	label := "proxy URL test passed"
	if kind == "hysteria2" {
		label = "End-to-end Hysteria2: PASS"
	}
	return Layer{
		Name:   endToEndName(kind, ep),
		Status: "PASS",
		Detail: fmt.Sprintf("%s (%dms)", label, d.Milliseconds()),
	}
}

func endToEndName(kind string, ep config.Endpoint) string {
	if kind == "hysteria2" {
		return fmt.Sprintf("End-to-end Hysteria2 %s", ep.Name)
	}
	return fmt.Sprintf("End-to-end %s", ep.Name)
}

func checkBackupRoutes(cfg *config.Config, routeName string) []Layer {
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	var out []Layer
	found := false
	for _, other := range cfg.Endpoints {
		if other.Name == routeName || other.Type != config.ProbeSingBox {
			continue
		}
		found = true
		timeout := cfg.TimeoutFor(other)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		d, err := client.Delay(ctx, other.Outbound, cfg.SingBox.TestURL, timeout)
		cancel()
		if err != nil {
			reason := errString(err)
			if strings.Contains(reason, "timeout") {
				reason = "timeout"
			}
			out = append(out, Layer{
				Name:   fmt.Sprintf("Backup %s", other.Name),
				Status: "FAIL",
				Detail: reason,
				Hint:   "this backup cannot carry traffic right now",
			})
			continue
		}
		out = append(out, Layer{
			Name:   fmt.Sprintf("Backup %s", other.Name),
			Status: "PASS",
			Detail: fmt.Sprintf("%dms", d.Milliseconds()),
		})
	}
	if !found {
		return []Layer{{Name: "Backup", Status: "SKIP", Detail: "no other proxy routes"}}
	}
	return out
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
	if err := conn.Close(); err != nil {
		return Layer{Name: fmt.Sprintf("Server %s", ep.Name), Status: "FAIL", Detail: "tcp connection close failed"}
	}
	return Layer{Name: fmt.Sprintf("Server %s", ep.Name), Status: "PASS", Detail: "server reachable"}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	s := redact.Error(err)
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
