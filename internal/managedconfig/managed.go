package managedconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/setup"
	"github.com/ilyasmohaimel/frostgate/internal/singbox"
)

const (
	selectorTag = "frostgate-selector"
	mixedTag    = "frostgate-mixed"
	directTag   = "direct"
	blockTag    = "block"
)

// Managed is the single source of truth for the Frostgate-managed sing-box
// configuration. It parses the on-disk sing-box.json into structured routes,
// preserving Frostgate-managed structural fields and any extra user outbounds.
type Managed struct {
	Dir            string
	Secret         string
	ControllerAddr string
	BenchmarkAddr  string
	VLESS          []*setup.VLESSOutbound
	Hysteria       []*setup.Hysteria2Outbound
	// Raw preserves top-level sing-box keys not managed by Frostgate.
	Raw         map[string]any
	managedTags map[string]bool
}

// SidecarRunner abstracts the systemd sidecar lifecycle so it can be tested
// without touching the real user session.
type SidecarRunner struct {
	// Validator validates the candidate config bytes. If nil, only JSON
	// validity is checked. The default runner wires this to `sing-box check`.
	Validator        func(candidate []byte) error
	DaemonReload     func() error
	Enable           func() error
	Disable          func() error
	Stop             func() error
	Restart          func() error
	IsActive         func() (bool, error)
	ControllerReady  func(expectedOutbounds []string) (bool, error)
	controllerSecret string
	controllerAddr   string
}

// DefaultSidecarRunner shells out to systemctl for the user session and
// verifies the controller is reachable with the expected outbounds.
func DefaultSidecarRunner(controllerAddr, secret string) *SidecarRunner {
	return DefaultSidecarRunnerForService(controllerAddr, secret, "frostgate-singbox.service")
}

// DefaultSidecarRunnerForService scopes lifecycle operations to one managed
// profile. Custom profiles must never restart the default sidecar unit.
func DefaultSidecarRunnerForService(controllerAddr, secret, service string) *SidecarRunner {
	if service == "" {
		service = "frostgate-singbox.service"
	}
	return &SidecarRunner{
		controllerAddr:   controllerAddr,
		controllerSecret: secret,
		Validator:        SingBoxCheckValidator(),
		DaemonReload: func() error {
			return runCmd("systemctl", "--user", "daemon-reload")
		},
		Enable: func() error {
			return runCmd("systemctl", "--user", "enable", service)
		},
		Disable: func() error {
			return runCmd("systemctl", "--user", "disable", service)
		},
		Stop: func() error {
			return runCmd("systemctl", "--user", "stop", service)
		},
		Restart: func() error {
			return runCmd("systemctl", "--user", "restart", service)
		},
		IsActive: func() (bool, error) {
			out, err := outputCmd("systemctl", "--user", "is-active", service)
			if err != nil {
				return false, err
			}
			return string(out) == "active\n" || string(out) == "active", nil
		},
		ControllerReady: func(expectedOutbounds []string) (bool, error) {
			deadline := time.Now().Add(6 * time.Second)
			var lastErr error
			for {
				ready, err := verifyController(controllerAddr, secret, expectedOutbounds)
				if err == nil && ready {
					return true, nil
				}
				lastErr = err
				if time.Now().After(deadline) {
					return false, lastErr
				}
				time.Sleep(150 * time.Millisecond)
			}
		},
	}
}

// SingBoxCheckValidator returns a validator that runs `sing-box check` on the
// candidate. If sing-box is not installed it returns a clear error rather than
// silently skipping validation.
func SingBoxCheckValidator() func([]byte) error {
	return func(candidate []byte) error {
		sb, err := lookPath("sing-box")
		if err != nil {
			return fmt.Errorf("sing-box not available; cannot validate managed config: %w", err)
		}
		f, err := os.CreateTemp("", "frostgate-check-*")
		if err != nil {
			return err
		}
		path := f.Name()
		if _, err := f.Write(candidate); err != nil {
			return errors.Join(err, f.Close(), os.Remove(path))
		}
		if err := f.Close(); err != nil {
			return errors.Join(err, os.Remove(path))
		}
		out, checkErr := outputCmd(sb, "check", "-c", path)
		removeErr := os.Remove(path)
		if checkErr != nil {
			return errors.Join(fmt.Errorf("sing-box check failed: %s", strings.TrimSpace(string(out))), removeErr)
		}
		if removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("remove temporary sing-box candidate: %w", removeErr)
		}
		return nil
	}
}

func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	return c.Run()
}

func outputCmd(name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	return c.Output()
}

func verifyController(addr, secret string, expected []string) (bool, error) {
	client := singbox.New(addr, secret)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	outbounds, err := client.Outbounds(ctx)
	if err != nil {
		return false, err
	}
	have := map[string]bool{}
	for _, o := range outbounds {
		have[o] = true
	}
	for _, e := range expected {
		if !have[e] {
			return false, fmt.Errorf("missing expected outbound %q", e)
		}
	}
	return true, nil
}

func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func Load(dir string) (*Managed, error) {
	m := &Managed{Dir: dir, Raw: map[string]any{}, managedTags: map[string]bool{}}
	if data, err := readManagedStateFile(dir, "controller-secret"); err == nil {
		m.Secret = strings.TrimSpace(string(data))
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read controller secret: %w", err)
	}
	data, err := readManagedStateFile(dir, "sing-box.json")
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	m.Raw = raw
	if m.Secret == "" {
		experimental, _ := raw["experimental"].(map[string]any)
		clash, _ := experimental["clash_api"].(map[string]any)
		m.Secret = inboundString(clash, "secret")
	}
	m.parseOutbounds(raw)
	return m, nil
}

func readManagedStateFile(dir, name string) ([]byte, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to read symlinked managed state file %s", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("managed state file %s is not regular", path)
	}
	return os.ReadFile(path)
}

func (m *Managed) parseOutbounds(raw map[string]any) {
	for _, ob := range rawOutbounds(raw) {
		mm, ok := ob.(map[string]any)
		if !ok || inboundString(mm, "tag") != selectorTag {
			continue
		}
		for _, tag := range stringSlice(mm["outbounds"]) {
			m.managedTags[tag] = true
		}
	}
	outs, _ := raw["outbounds"].([]any)
	for _, ob := range outs {
		mm, ok := ob.(map[string]any)
		if !ok {
			continue
		}
		t, _ := mm["type"].(string)
		tag, _ := mm["tag"].(string)
		switch t {
		case "vless":
			if m.managedTags[tag] {
				m.VLESS = append(m.VLESS, parseVLESS(mm, tag))
			}
		case "hysteria2":
			if m.managedTags[tag] {
				m.Hysteria = append(m.Hysteria, parseHysteria(mm, tag))
			}
		}
	}
}

func parseVLESS(mm map[string]any, tag string) *setup.VLESSOutbound {
	v := &setup.VLESSOutbound{Tag: tag}
	v.Server, _ = mm["server"].(string)
	if pf, ok := mm["server_port"].(float64); ok {
		v.ServerPort = int(pf)
	}
	v.UUID, _ = mm["uuid"].(string)
	v.Flow, _ = mm["flow"].(string)
	v.PacketEncoding, _ = mm["packet_encoding"].(string)
	_, v.PacketEncodingSet = mm["packet_encoding"]
	v.Network, _ = mm["network"].(string)
	if tlsRaw, ok := mm["tls"].(map[string]any); ok {
		v.SNI, _ = tlsRaw["server_name"].(string)
		if utls, ok := tlsRaw["utls"].(map[string]any); ok {
			v.FP, _ = utls["fingerprint"].(string)
		}
		if reality, ok := tlsRaw["reality"].(map[string]any); ok {
			v.PBK, _ = reality["public_key"].(string)
			v.SID, _ = reality["short_id"].(string)
			v.SpiderX, _ = reality["spider_x"].(string)
		}
	}
	return v
}

func parseHysteria(mm map[string]any, tag string) *setup.Hysteria2Outbound {
	h := &setup.Hysteria2Outbound{Tag: tag}
	h.Server, _ = mm["server"].(string)
	if pf, ok := mm["server_port"].(float64); ok {
		h.ServerPort = int(pf)
	}
	h.Password, _ = mm["password"].(string)
	h.Network, _ = mm["network"].(string)
	if obfs, ok := mm["obfs"].(map[string]any); ok {
		h.ObfsType, _ = obfs["type"].(string)
		h.ObfsPassword, _ = obfs["password"].(string)
	}
	if tlsRaw, ok := mm["tls"].(map[string]any); ok {
		h.SNI, _ = tlsRaw["server_name"].(string)
		if v, ok := tlsRaw["insecure"].(bool); ok {
			h.Insecure = v
		}
	}
	return h
}

// ConfigureProfile supplies bindings for a new Frostgate-managed sidecar. An
// existing owned sidecar keeps its recorded bindings during regeneration.
func (m *Managed) ConfigureProfile(controllerAddr, benchmarkAddr string) {
	m.ControllerAddr = controllerHostPort(controllerAddr)
	m.BenchmarkAddr = benchmarkAddr
}

func (m *Managed) HasManagedSidecar() bool {
	return len(m.managedTags) > 0 || m.BenchmarkProxyPresent() || hasSelector(m.Raw)
}

// UsesProfileBindings reports whether an existing managed sidecar already
// owns the supplied controller and benchmark listener. A running custom
// profile must be updated in place rather than rejected because its own ports
// are occupied.
func (m *Managed) UsesProfileBindings(controller, benchmark string) bool {
	if !sameController(m.clashController(), controller) {
		return false
	}
	addr, ok := m.BenchmarkProxy()
	return ok && addr == benchmark
}

func (m *Managed) BenchmarkProxyPresent() bool {
	_, ok := m.BenchmarkProxy()
	return ok
}

// Generate mutates only Frostgate-owned objects. Unrelated top-level keys,
// inbounds, route rules, log settings and protocol outbounds survive intact.
func (m *Managed) Generate() ([]byte, error) {
	if err := m.validateManagedRoutes(); err != nil {
		return nil, err
	}
	secret := m.Secret
	if secret == "" {
		s, err := setup.GenerateSecret()
		if err != nil {
			return nil, err
		}
		secret = s
		m.Secret = secret
	}
	top, err := cloneRaw(m.Raw)
	if err != nil {
		return nil, fmt.Errorf("clone existing config: %w", err)
	}
	hasProxy := len(m.VLESS)+len(m.Hysteria) > 0
	tags := m.ProxyTags()
	updated := map[string]map[string]any{}
	for _, v := range m.VLESS {
		updated[v.Tag] = vlessMap(v)
	}
	for _, h := range m.Hysteria {
		updated[h.Tag] = hysteriaMap(h)
	}

	outs := rawOutbounds(top)
	seen := map[string]bool{}
	selectorIndex := -1
	directSeen, blockSeen := false, false
	for i, raw := range outs {
		mm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		tag := inboundString(mm, "tag")
		if tag == directTag {
			directSeen = true
		}
		if tag == blockTag {
			blockSeen = true
		}
		if tag == selectorTag {
			selectorIndex = i
		}
		if replacement, owned := updated[tag]; owned {
			outs[i] = replacement
			seen[tag] = true
		}
	}
	for _, tag := range tags {
		if !seen[tag] {
			outs = append(outs, updated[tag])
		}
	}
	if hasProxy {
		if !directSeen {
			outs = append([]any{map[string]any{"type": "direct", "tag": directTag}}, outs...)
			if selectorIndex >= 0 {
				selectorIndex++
			}
		}
		if !blockSeen {
			outs = append([]any{map[string]any{"type": "block", "tag": blockTag}}, outs...)
			if selectorIndex >= 0 {
				selectorIndex++
			}
		}
		selector := map[string]any{"type": "selector", "tag": selectorTag}
		if selectorIndex >= 0 {
			if existing, ok := outs[selectorIndex].(map[string]any); ok {
				selector = existing
			}
		} else {
			selectorIndex = len(outs)
			outs = append(outs, selector)
		}
		selector["outbounds"] = stringsToAny(tags)
		if current := inboundString(selector, "default"); !containsString(tags, current) {
			selector["default"] = tags[0]
		}
		outs[selectorIndex] = selector
	}
	top["outbounds"] = outs
	if hasProxy {
		if err := m.ensureOwnedInfrastructure(top); err != nil {
			return nil, err
		}
	}
	if _, ok := top["log"]; !ok {
		top["log"] = map[string]any{"level": "warn", "output": filepath.Join(m.Dir, "sing-box.log")}
	}
	return json.MarshalIndent(top, "", "  ")
}

func (m *Managed) ensureOwnedInfrastructure(top map[string]any) error {
	controller := m.ControllerAddr
	if controller == "" {
		controller = "127.0.0.1:9090"
	}
	benchmark := m.BenchmarkAddr
	if benchmark == "" {
		if addr, ok := m.BenchmarkProxy(); ok {
			benchmark = addr
		} else {
			benchmark = "127.0.0.1:2080"
		}
	}
	host, portStr, err := net.SplitHostPort(benchmark)
	if err != nil {
		return fmt.Errorf("invalid managed benchmark address %q: %w", benchmark, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid managed benchmark port %q", portStr)
	}
	if !isLoopback(host) {
		return fmt.Errorf("managed benchmark listener must be loopback, got %q", host)
	}
	inbounds, _ := top["inbounds"].([]any)
	foundInbound := false
	for i, raw := range inbounds {
		inbound, ok := raw.(map[string]any)
		if !ok || inboundString(inbound, "tag") != mixedTag {
			continue
		}
		foundInbound = true
		inbounds[i] = inbound
	}
	if !foundInbound {
		inbounds = append(inbounds, map[string]any{"type": "mixed", "tag": mixedTag, "listen": host, "listen_port": port})
	} else if m.BenchmarkAddr != "" {
		for i, raw := range inbounds {
			inbound, ok := raw.(map[string]any)
			if !ok || inboundString(inbound, "tag") != mixedTag {
				continue
			}
			inbound["listen"] = host
			inbound["listen_port"] = port
			inbounds[i] = inbound
		}
	}
	top["inbounds"] = inbounds

	route, _ := top["route"].(map[string]any)
	if route == nil {
		route = map[string]any{"final": directTag}
	}
	rules, _ := route["rules"].([]any)
	foundRule := false
	for i, raw := range rules {
		rule, ok := raw.(map[string]any)
		if ok && inboundString(rule, "inbound") == mixedTag {
			rule["outbound"] = selectorTag
			rules[i] = rule
			foundRule = true
		}
	}
	if !foundRule {
		rules = append(rules, map[string]any{"inbound": mixedTag, "outbound": selectorTag})
	}
	route["rules"] = rules
	top["route"] = route

	experimental, _ := top["experimental"].(map[string]any)
	if experimental == nil {
		experimental = map[string]any{}
	}
	clash, _ := experimental["clash_api"].(map[string]any)
	if clash == nil {
		clash = map[string]any{}
	}
	if m.ControllerAddr != "" || inboundString(clash, "external_controller") == "" {
		clash["external_controller"] = controller
	}
	if inboundString(clash, "secret") == "" {
		clash["secret"] = m.Secret
	}
	experimental["clash_api"] = clash
	if _, ok := experimental["cache_file"]; !ok {
		experimental["cache_file"] = map[string]any{"enabled": true, "path": filepath.Join(m.Dir, "cache.db")}
	}
	top["experimental"] = experimental
	return nil
}

func (m *Managed) validateManagedRoutes() error {
	seen := make(map[string]struct{}, len(m.VLESS)+len(m.Hysteria))
	add := func(tag string) error {
		if tag == "" {
			return fmt.Errorf("managed outbound has an empty tag")
		}
		if _, exists := seen[tag]; exists {
			return fmt.Errorf("duplicate managed outbound tag %q", tag)
		}
		seen[tag] = struct{}{}
		return nil
	}
	for _, v := range m.VLESS {
		if v == nil {
			return fmt.Errorf("nil managed VLESS outbound")
		}
		if err := add(v.Tag); err != nil {
			return err
		}
		if v.Server == "" || v.ServerPort < 1 || v.ServerPort > 65535 || v.UUID == "" || v.SNI == "" || v.PBK == "" {
			return fmt.Errorf("managed VLESS outbound %q is incomplete", v.Tag)
		}
		if v.Network != "" && !strings.EqualFold(v.Network, "tcp") {
			return fmt.Errorf("managed VLESS outbound %q uses unsupported network", v.Tag)
		}
	}
	for _, h := range m.Hysteria {
		if h == nil {
			return fmt.Errorf("nil managed Hysteria2 outbound")
		}
		if err := add(h.Tag); err != nil {
			return err
		}
		if h.Server == "" || h.ServerPort < 1 || h.ServerPort > 65535 || h.Password == "" {
			return fmt.Errorf("managed Hysteria2 outbound %q is incomplete", h.Tag)
		}
		if h.ObfsType != "" && h.ObfsType != "salamander" {
			return fmt.Errorf("managed Hysteria2 outbound %q uses unsupported obfs", h.Tag)
		}
		if h.ObfsType == "salamander" && h.ObfsPassword == "" {
			return fmt.Errorf("managed Hysteria2 outbound %q has salamander without a password", h.Tag)
		}
		if h.Network != "" && !strings.EqualFold(h.Network, "tcp") && !strings.EqualFold(h.Network, "udp") {
			return fmt.Errorf("managed Hysteria2 outbound %q uses unsupported network", h.Tag)
		}
	}
	rawTags := map[string]int{}
	for _, raw := range rawOutbounds(m.Raw) {
		outbound, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		tag := inboundString(outbound, "tag")
		if tag == "" {
			continue
		}
		rawTags[tag]++
		if _, exists := seen[tag]; exists && !m.managedTags[tag] {
			return fmt.Errorf("managed outbound tag %q collides with an unrelated outbound", tag)
		}
	}
	for tag, count := range rawTags {
		if count > 1 {
			return fmt.Errorf("duplicate outbound tag %q in managed config", tag)
		}
	}
	return nil
}

func rawOutbounds(top map[string]any) []any {
	outs, _ := top["outbounds"].([]any)
	return append([]any(nil), outs...)
}

func stringSlice(raw any) []string {
	items, _ := raw.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stringsToAny(items []string) []any {
	out := make([]any, len(items))
	for i := range items {
		out[i] = items[i]
	}
	return out
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func hasSelector(top map[string]any) bool {
	for _, raw := range rawOutbounds(top) {
		if outbound, ok := raw.(map[string]any); ok && inboundString(outbound, "tag") == selectorTag {
			return true
		}
	}
	return false
}

func cloneRaw(raw map[string]any) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func controllerHostPort(raw string) string {
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

func vlessMap(v *setup.VLESSOutbound) map[string]any {
	tlsCfg := map[string]any{
		"enabled":     true,
		"server_name": v.SNI,
		"utls":        map[string]any{"enabled": true, "fingerprint": orDefault(v.FP, "chrome")},
		"reality":     map[string]any{"enabled": true, "public_key": v.PBK, "short_id": v.SID},
	}
	if v.SpiderX != "" {
		tlsCfg["reality"].(map[string]any)["spider_x"] = v.SpiderX
	}
	pe := v.PacketEncoding
	if pe == "" && !v.PacketEncodingSet {
		pe = "xudp"
	}
	out := map[string]any{
		"type":            "vless",
		"tag":             v.Tag,
		"server":          v.Server,
		"server_port":     v.ServerPort,
		"uuid":            v.UUID,
		"flow":            v.Flow,
		"packet_encoding": pe,
		"tls":             tlsCfg,
	}
	if v.Network != "" {
		out["network"] = v.Network
	}
	return out
}

func hysteriaMap(h *setup.Hysteria2Outbound) map[string]any {
	tlsCfg := map[string]any{"enabled": true, "server_name": h.SNI}
	if h.Insecure {
		tlsCfg["insecure"] = true
	}
	ob := map[string]any{
		"type":        "hysteria2",
		"tag":         h.Tag,
		"server":      h.Server,
		"server_port": h.ServerPort,
		"password":    h.Password,
		"tls":         tlsCfg,
	}
	if h.ObfsPassword != "" {
		obfsType := h.ObfsType
		if obfsType == "" {
			obfsType = "salamander"
		}
		ob["obfs"] = map[string]any{"type": obfsType, "password": h.ObfsPassword}
	}
	if h.Network != "" {
		ob["network"] = h.Network
	}
	return ob
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (m *Managed) ProxyTags() []string {
	var tags []string
	for _, v := range m.VLESS {
		tags = append(tags, v.Tag)
	}
	for _, h := range m.Hysteria {
		tags = append(tags, h.Tag)
	}
	return tags
}

func (m *Managed) AddVLESS(v *setup.VLESSOutbound)        { m.VLESS = append(m.VLESS, v) }
func (m *Managed) AddHysteria(h *setup.Hysteria2Outbound) { m.Hysteria = append(m.Hysteria, h) }

// SaveTransaction updates only the managed sing-box files. Callers that also
// change Frostgate's config.yaml should use SaveTransactionWithConfig so both
// files are rolled back together.
func (m *Managed) SaveTransaction(runner *SidecarRunner) error {
	return m.saveTransaction(runner, "", nil)
}

// SaveTransactionWithConfig atomically installs the managed sing-box candidate
// and a validated Frostgate configuration. If the sidecar cannot accept the
// candidate, both files are restored.
func (m *Managed) SaveTransactionWithConfig(runner *SidecarRunner, configPath string, configData []byte) error {
	if configPath == "" {
		return fmt.Errorf("config path is required")
	}
	if len(configData) == 0 {
		return fmt.Errorf("config candidate is empty")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(configData, &doc); err != nil {
		return fmt.Errorf("config candidate is not valid YAML: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config candidate must be a YAML mapping")
	}
	return m.saveTransaction(runner, configPath, configData)
}

type transactionFile struct {
	path          string
	candidate     []byte
	install       bool
	followSymlink bool
}

type fileSnapshot struct {
	path          string
	data          []byte
	exists        bool
	followSymlink bool
}

func (m *Managed) saveTransaction(runner *SidecarRunner, configPath string, configData []byte) error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	candidate, err := m.Generate()
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	if !json.Valid(candidate) {
		return fmt.Errorf("candidate is not valid JSON")
	}
	if runner != nil && runner.Validator != nil {
		if err := runner.Validator(candidate); err != nil {
			return fmt.Errorf("validation: %w", err)
		}
	}

	files := []transactionFile{
		{path: filepath.Join(m.Dir, "sing-box.json"), candidate: candidate, install: true},
		{path: filepath.Join(m.Dir, "controller-secret"), candidate: []byte(m.Secret + "\n"), install: m.Secret != ""},
	}
	if configPath != "" {
		// A config symlink is a profile alias and must keep pointing at the
		// canonical config. Managed-state files are never followed: allowing a
		// hostile or stale state symlink would let one profile overwrite another.
		files = append(files, transactionFile{path: configPath, candidate: configData, install: true, followSymlink: true})
	}

	snapshots := make([]fileSnapshot, 0, len(files))
	for _, f := range files {
		s := fileSnapshot{path: f.path, followSymlink: f.followSymlink}
		if info, lstatErr := os.Lstat(f.path); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			if !f.followSymlink {
				return fmt.Errorf("snapshot %s: refusing symlinked managed state file", f.path)
			}
			if _, resolveErr := filepath.EvalSymlinks(f.path); resolveErr != nil {
				return fmt.Errorf("snapshot %s: resolve config symlink: %w", f.path, resolveErr)
			}
		} else if lstatErr != nil && !os.IsNotExist(lstatErr) {
			return fmt.Errorf("snapshot %s: %w", f.path, lstatErr)
		}
		if data, readErr := os.ReadFile(f.path); readErr == nil {
			s.data = data
			s.exists = true
		} else if !os.IsNotExist(readErr) {
			return fmt.Errorf("snapshot %s: %w", f.path, readErr)
		}
		snapshots = append(snapshots, s)
	}

	for _, f := range files {
		if !f.install {
			continue
		}
		if err := atomicWrite(f.path, f.candidate, 0o600, f.followSymlink); err != nil {
			original := fmt.Errorf("install %s: %w", f.path, err)
			if rollbackErr := rollbackSnapshots(snapshots); rollbackErr != nil {
				return errors.Join(original, fmt.Errorf("rollback files: %w", rollbackErr))
			}
			return original
		}
	}
	rollback := func(err error) error {
		var recovery error
		if rollbackErr := rollbackSnapshots(snapshots); rollbackErr != nil {
			recovery = errors.Join(recovery, fmt.Errorf("rollback files: %w", rollbackErr))
		}
		if runner != nil && runner.Restart != nil {
			if restartErr := runner.Restart(); restartErr != nil {
				recovery = errors.Join(recovery, fmt.Errorf("restart restored sidecar: %w", restartErr))
			}
		}
		if recovery != nil {
			return errors.Join(err, recovery)
		}
		return err
	}

	// Restart sidecar + verify.
	if runner != nil {
		if runner.DaemonReload != nil {
			if err := runner.DaemonReload(); err != nil {
				return rollback(fmt.Errorf("daemon-reload failed: %w", err))
			}
		}
		if runner.Restart != nil {
			if err := runner.Restart(); err != nil {
				return rollback(fmt.Errorf("sidecar restart failed: %w", err))
			}
		}
		if runner.IsActive != nil {
			active, err := runner.IsActive()
			if err != nil {
				return rollback(fmt.Errorf("is-active check error: %w", err))
			}
			if !active {
				return rollback(fmt.Errorf("sidecar not active after restart"))
			}
		}
		if runner.ControllerReady != nil {
			ok, err := runner.ControllerReady(m.ProxyTags())
			if err != nil || !ok {
				if err == nil {
					err = fmt.Errorf("expected outbounds unavailable")
				}
				return rollback(fmt.Errorf("controller verification failed: %w", err))
			}
		}
		if runner.Enable != nil {
			if err := runner.Enable(); err != nil {
				return rollback(fmt.Errorf("sidecar enable failed: %w", err))
			}
		}
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomicWithOptions(path, data, mode, false)
}

func writeConfigFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomicWithOptions(path, data, mode, true)
}

func writeFileAtomicWithOptions(path string, data []byte, mode os.FileMode, followSymlink bool) (err error) {
	// Config aliases are profile aliases, not files Frostgate may replace. An
	// atomic rename over a symlink would silently sever that identity; resolve
	// an existing config link and update its target instead. Managed state is
	// deliberately different: do not follow its links across profile bounds.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if !followSymlink {
			return fmt.Errorf("refusing to write symlinked managed state file %s", path)
		}
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			return fmt.Errorf("resolve symlink %s: %w", path, resolveErr)
		}
		path = resolved
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".frostgate-cand-*")
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
			err = errors.Join(err, fmt.Errorf("remove temporary config %s: %w", tmpPath, removeErr))
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

var (
	atomicWriteFile       = writeFileAtomic
	atomicWriteConfigFile = writeConfigFileAtomic
	removeFile            = os.Remove
)

func atomicWrite(path string, data []byte, mode os.FileMode, followSymlink bool) error {
	if followSymlink {
		return atomicWriteConfigFile(path, data, mode)
	}
	return atomicWriteFile(path, data, mode)
}

func rollbackSnapshots(snapshots []fileSnapshot) error {
	var errs []error
	for _, snapshot := range snapshots {
		if snapshot.exists {
			if err := atomicWrite(snapshot.path, snapshot.data, 0o600, snapshot.followSymlink); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", snapshot.path, err))
			}
		} else {
			if err := removeFile(snapshot.path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove %s: %w", snapshot.path, err))
			}
		}
	}
	return errors.Join(errs...)
}
