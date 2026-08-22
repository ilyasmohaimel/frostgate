package managedconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
	Dir      string
	Secret   string
	VLESS    []*setup.VLESSOutbound
	Hysteria []*setup.Hysteria2Outbound
	// Raw preserves top-level sing-box keys not managed by Frostgate.
	Raw map[string]any
	// extraOutbounds preserves user-defined outbounds that Frostgate does
	// not manage (anything other than direct/block/proxy/selector).
	extraOutbounds []map[string]any
}

// SidecarRunner abstracts the systemd sidecar lifecycle so it can be tested
// without touching the real user session.
type SidecarRunner struct {
	// Validator validates the candidate config bytes. If nil, only JSON
	// validity is checked. The default runner wires this to `sing-box check`.
	Validator        func(candidate []byte) error
	DaemonReload     func() error
	Restart          func() error
	IsActive         func() (bool, error)
	ControllerReady  func(expectedOutbounds []string) (bool, error)
	controllerSecret string
	controllerAddr   string
}

// DefaultSidecarRunner shells out to systemctl for the user session and
// verifies the controller is reachable with the expected outbounds.
func DefaultSidecarRunner(controllerAddr, secret string) *SidecarRunner {
	return &SidecarRunner{
		controllerAddr:   controllerAddr,
		controllerSecret: secret,
		Validator:        SingBoxCheckValidator(),
		DaemonReload: func() error {
			return runCmd("systemctl", "--user", "daemon-reload")
		},
		Restart: func() error {
			return runCmd("systemctl", "--user", "restart", "frostgate-singbox.service")
		},
		IsActive: func() (bool, error) {
			out, err := outputCmd("systemctl", "--user", "is-active", "frostgate-singbox.service")
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
			f.Close()
			os.Remove(path)
			return err
		}
		f.Close()
		defer os.Remove(path)
		if out, err := outputCmd(sb, "check", "-c", path); err != nil {
			return fmt.Errorf("sing-box check failed: %s", strings.TrimSpace(string(out)))
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
	m := &Managed{Dir: dir, Raw: map[string]any{}}
	if data, err := os.ReadFile(filepath.Join(dir, "controller-secret")); err == nil {
		m.Secret = strings.TrimSpace(string(data))
	}
	data, err := os.ReadFile(filepath.Join(dir, "sing-box.json"))
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
	m.parseOutbounds(raw)
	return m, nil
}

func (m *Managed) parseOutbounds(raw map[string]any) {
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
			m.VLESS = append(m.VLESS, parseVLESS(mm, tag))
		case "hysteria2":
			m.Hysteria = append(m.Hysteria, parseHysteria(mm, tag))
		default:
			if tag == directTag || tag == blockTag || tag == selectorTag || tag == mixedTag {
				continue
			}
			m.extraOutbounds = append(m.extraOutbounds, mm)
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

// Generate rebuilds the canonical Frostgate sing-box.json preserving extra
// user outbounds and non-managed top-level keys.
func (m *Managed) Generate() ([]byte, error) {
	secret := m.Secret
	if secret == "" {
		s, err := setup.GenerateSecret()
		if err != nil {
			return nil, err
		}
		secret = s
		m.Secret = secret
	}
	hasProxy := len(m.VLESS)+len(m.Hysteria) > 0
	outbounds := []any{
		map[string]any{"type": "direct", "tag": directTag},
		map[string]any{"type": "block", "tag": blockTag},
	}
	if hasProxy {
		var tags []string
		for _, v := range m.VLESS {
			tags = append(tags, v.Tag)
			outbounds = append(outbounds, vlessMap(v))
		}
		for _, h := range m.Hysteria {
			tags = append(tags, h.Tag)
			outbounds = append(outbounds, hysteriaMap(h))
		}
		selector := map[string]any{
			"type":      "selector",
			"tag":       selectorTag,
			"outbounds": tags,
		}
		if len(tags) > 0 {
			selector["default"] = tags[0]
		}
		outbounds = append(outbounds, selector)
	}
	for _, eo := range m.extraOutbounds {
		outbounds = append(outbounds, eo)
	}

	top := map[string]any{}
	for k, v := range m.Raw {
		top[k] = v
	}
	top["outbounds"] = outbounds
	if hasProxy {
		top["inbounds"] = []any{
			map[string]any{
				"type":        "mixed",
				"tag":         mixedTag,
				"listen":      "127.0.0.1",
				"listen_port": 2080,
			},
		}
		top["route"] = map[string]any{
			"rules": []any{
				map[string]any{"inbound": mixedTag, "outbound": selectorTag},
			},
			"final": directTag,
		}
	}
	log := map[string]any{"level": "warn", "output": filepath.Join(m.Dir, "sing-box.log")}
	top["log"] = log
	cfg := map[string]any{}
	for k, v := range m.Raw {
		if k == "outbounds" || k == "inbounds" || k == "route" || k == "log" {
			continue
		}
		cfg[k] = v
	}
	for k, v := range top {
		cfg[k] = v
	}
	return json.MarshalIndent(cfg, "", "  ")
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
	if pe == "" {
		pe = "xudp"
	}
	return map[string]any{
		"type":            "vless",
		"tag":             v.Tag,
		"server":          v.Server,
		"server_port":     v.ServerPort,
		"uuid":            v.UUID,
		"flow":            v.Flow,
		"packet_encoding": pe,
		"tls":             tlsCfg,
	}
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

// SaveTransaction performs a transactional write: snapshot current files,
// write the candidate atomically, validate it, restart the sidecar and verify
// the controller exposes the expected outbounds. On any failure after mutation
// it restores the previous files.
func (m *Managed) SaveTransaction(runner *SidecarRunner) error {
	dir := m.Dir
	files := []string{"sing-box.json", "controller-secret", "config.yaml"}
	snapDir, err := os.MkdirTemp("", "frostgate-snap-*")
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	defer os.RemoveAll(snapDir)
	for _, f := range files {
		if data, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			if err := os.WriteFile(filepath.Join(snapDir, f), data, 0o600); err != nil {
				return fmt.Errorf("snapshot %s: %w", f, err)
			}
		}
	}

	candidate, err := m.Generate()
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	// Validate candidate: JSON valid (always) + injected validator (sing-box).
	if !json.Valid(candidate) {
		return fmt.Errorf("candidate is not valid JSON")
	}
	if runner != nil && runner.Validator != nil {
		if err := runner.Validator(candidate); err != nil {
			return fmt.Errorf("validation: %w", err)
		}
	}

	// Atomically install candidate.
	tmp, err := os.CreateTemp(dir, "frostgate-cand-*")
	if err != nil {
		return fmt.Errorf("candidate temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(candidate); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write candidate: %w", err)
	}
	tmp.Close()
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod candidate: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, "sing-box.json")); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("install candidate: %w", err)
	}
	if m.Secret != "" {
		if err := os.WriteFile(filepath.Join(dir, "controller-secret"), []byte(m.Secret+"\n"), 0o600); err != nil {
			rollbackFiles(dir, snapDir, files)
			return fmt.Errorf("write secret: %w", err)
		}
	}
	rollback := func(err error) error {
		rollbackFiles(dir, snapDir, files)
		if runner != nil && runner.Restart != nil {
			_ = runner.Restart()
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
	}
	return nil
}

func rollbackFiles(dir, snapDir string, files []string) {
	for _, f := range files {
		snap := filepath.Join(snapDir, f)
		if data, err := os.ReadFile(snap); err == nil {
			_ = os.WriteFile(filepath.Join(dir, f), data, 0o600)
		} else {
			_ = os.Remove(filepath.Join(dir, f))
		}
	}
}
