package managedconfig

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// BenchmarkProxy returns the loopback address of Frostgate's managed mixed or
// SOCKS inbound. It accepts only Frostgate's tag and route wiring, so an
// arbitrary sing-box controller never causes Frostgate to guess a listener.
func (m *Managed) BenchmarkProxy() (string, bool) {
	inbounds, _ := m.Raw["inbounds"].([]any)
	for _, raw := range inbounds {
		inbound, ok := raw.(map[string]any)
		if !ok || inboundString(inbound, "tag") != mixedTag {
			continue
		}
		kind := inboundString(inbound, "type")
		if kind != "mixed" && kind != "socks" {
			continue
		}
		host := inboundString(inbound, "listen")
		port, ok := inboundPort(inbound["listen_port"])
		if !ok || !isLoopback(host) || !m.routesInboundToSelector(mixedTag) {
			continue
		}
		return net.JoinHostPort(host, strconv.Itoa(port)), true
	}
	return "", false
}

// DiscoverBenchmarkProxy reads an existing managed listener without changing
// configuration or contacting a sidecar.
func DiscoverBenchmarkProxy(dir string) (string, bool) {
	m, err := Load(dir)
	if err != nil {
		return "", false
	}
	return m.BenchmarkProxy()
}

// IsLegacyManagedBenchmarkConfig reports whether this is the pre-benchmark
// Frostgate sidecar layout. The checks deliberately require Frostgate's
// controller and all configured proxy tags; external controllers are not
// migrated or assigned a guessed listener.
func (m *Managed) IsLegacyManagedBenchmarkConfig(controller string, configuredOutbounds []string) bool {
	if _, ok := m.BenchmarkProxy(); ok || len(configuredOutbounds) == 0 {
		return false
	}
	if !sameController(m.clashController(), controller) {
		return false
	}
	managed := map[string]bool{}
	for _, tag := range m.legacyProxyTags() {
		managed[tag] = true
	}
	if len(managed) != len(configuredOutbounds) {
		return false
	}
	for _, tag := range configuredOutbounds {
		if !managed[tag] {
			return false
		}
	}
	return true
}

// EnsureBenchmarkProxy discovers a managed benchmark listener, or upgrades a
// verified legacy Frostgate sidecar to the managed selector/inbound layout.
// It returns an empty address for unmanaged or ambiguous configurations.
func EnsureBenchmarkProxy(dir, controller string, configuredOutbounds []string, runner *SidecarRunner) (string, bool, error) {
	m, err := Load(dir)
	if err != nil {
		return "", false, nil
	}
	if addr, ok := m.BenchmarkProxy(); ok {
		return addr, false, nil
	}
	if !m.IsLegacyManagedBenchmarkConfig(controller, configuredOutbounds) {
		return "", false, nil
	}
	if runner == nil {
		return "", false, fmt.Errorf("managed benchmark migration requires a sidecar runner")
	}
	m.AdoptLegacyRoutes()
	if err := m.SaveTransaction(runner); err != nil {
		return "", false, fmt.Errorf("managed benchmark migration: %w", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		return "", false, fmt.Errorf("reload migrated managed config: %w", err)
	}
	addr, ok := reloaded.BenchmarkProxy()
	if !ok {
		return "", false, fmt.Errorf("migrated managed config has no benchmark listener")
	}
	return addr, true, nil
}

func (m *Managed) legacyProxyTags() []string {
	var tags []string
	for _, raw := range rawOutbounds(m.Raw) {
		outbound, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeName := inboundString(outbound, "type")
		if typeName == "vless" || typeName == "hysteria2" {
			tags = append(tags, inboundString(outbound, "tag"))
		}
	}
	return tags
}

// AdoptLegacyRoutes marks a sidecar that has already passed
// IsLegacyManagedBenchmarkConfig as Frostgate-owned before it is regenerated.
// Callers must not use it for an arbitrary external configuration.
func (m *Managed) AdoptLegacyRoutes() {
	if m.managedTags == nil {
		m.managedTags = map[string]bool{}
	}
	for _, raw := range rawOutbounds(m.Raw) {
		outbound, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		tag := inboundString(outbound, "tag")
		switch inboundString(outbound, "type") {
		case "vless":
			m.managedTags[tag] = true
			m.VLESS = append(m.VLESS, parseVLESS(outbound, tag))
		case "hysteria2":
			m.managedTags[tag] = true
			m.Hysteria = append(m.Hysteria, parseHysteria(outbound, tag))
		}
	}
}

func (m *Managed) routesInboundToSelector(tag string) bool {
	route, _ := m.Raw["route"].(map[string]any)
	rules, _ := route["rules"].([]any)
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if ok && inboundString(rule, "inbound") == tag && inboundString(rule, "outbound") == selectorTag {
			return true
		}
	}
	return false
}

func (m *Managed) clashController() string {
	experimental, _ := m.Raw["experimental"].(map[string]any)
	clash, _ := experimental["clash_api"].(map[string]any)
	return inboundString(clash, "external_controller")
}

func inboundString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func inboundPort(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n >= 1 && n <= 65535 && n == float64(int(n))
	case int:
		return n, n >= 1 && n <= 65535
	}
	return 0, false
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameController(left, right string) bool {
	left = controllerAddress(left)
	right = controllerAddress(right)
	return left != "" && left == right
}

func controllerAddress(raw string) string {
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
