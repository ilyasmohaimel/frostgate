package managedconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

func tempDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "mc-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func canonical(t *testing.T, data []byte) string {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func genBytes(t *testing.T, dir string, vless []*setup.VLESSOutbound, hyst []*setup.Hysteria2Outbound) []byte {
	t.Helper()
	m := &Managed{Dir: dir}
	for _, v := range vless {
		m.AddVLESS(v)
	}
	for _, h := range hyst {
		m.AddHysteria(h)
	}
	b, err := m.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeConfig(t *testing.T, dir string, vless []*setup.VLESSOutbound, hyst []*setup.Hysteria2Outbound) []byte {
	b := genBytes(t, dir, vless, hyst)
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTripVLESSReality(t *testing.T) {
	d := tempDir(t)
	v := &setup.VLESSOutbound{
		Tag: "reality1", Server: "203.0.113.1", ServerPort: 443, UUID: "u", Flow: "xtls-rprx-vision",
		SNI: "sni.example", FP: "chrome", PBK: "pub", SID: "sid", SpiderX: "/",
	}
	first := writeConfig(t, d, []*setup.VLESSOutbound{v}, nil)
	loaded, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.VLESS) != 1 {
		t.Fatalf("expected 1 vless, got %d", len(loaded.VLESS))
	}
	gv := loaded.VLESS[0]
	if gv.SpiderX != "/" || gv.PBK != "pub" || gv.SID != "sid" || gv.FP != "chrome" || gv.Flow != "xtls-rprx-vision" {
		t.Fatalf("vless fields lost: %+v", gv)
	}
	second, err := loaded.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if canonical(t, first) != canonical(t, second) {
		t.Fatalf("round-trip not stable:\n%s\n---\n%s", first, second)
	}
}

func TestRoundTripPacketEncoding(t *testing.T) {
	d := tempDir(t)
	v := &setup.VLESSOutbound{Tag: "vpe", Server: "203.0.113.2", ServerPort: 443, UUID: "u", SNI: "s", PBK: "p", FP: "chrome", PacketEncoding: "xudp"}
	writeConfig(t, d, []*setup.VLESSOutbound{v}, nil)
	loaded, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.VLESS[0].PacketEncoding != "xudp" {
		t.Fatalf("packet_encoding lost: %q", loaded.VLESS[0].PacketEncoding)
	}
}

func TestRoundTripHysteriaSalamander(t *testing.T) {
	d := tempDir(t)
	h := &setup.Hysteria2Outbound{Tag: "h1", Server: "203.0.113.3", ServerPort: 8443, Password: "pw", ObfsType: "salamander", ObfsPassword: "ob", SNI: "h.example"}
	writeConfig(t, d, nil, []*setup.Hysteria2Outbound{h})
	loaded, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	gh := loaded.Hysteria[0]
	if gh.ObfsType != "salamander" || gh.ObfsPassword != "ob" || gh.SNI != "h.example" {
		t.Fatalf("hysteria fields lost: %+v", gh)
	}
}

func TestRoundTripHysteriaInsecure(t *testing.T) {
	d := tempDir(t)
	h := &setup.Hysteria2Outbound{Tag: "h2", Server: "203.0.113.4", ServerPort: 443, Password: "pw", Insecure: true}
	writeConfig(t, d, nil, []*setup.Hysteria2Outbound{h})
	loaded, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Hysteria[0].Insecure {
		t.Fatalf("insecure lost")
	}
}

func TestRoundTripMultipleRoutes(t *testing.T) {
	d := tempDir(t)
	v1 := &setup.VLESSOutbound{Tag: "v1", Server: "203.0.113.5", ServerPort: 443, UUID: "u1", SNI: "s1", PBK: "p1", FP: "chrome"}
	v2 := &setup.VLESSOutbound{Tag: "v2", Server: "203.0.113.6", ServerPort: 443, UUID: "u2", SNI: "s2", PBK: "p2", FP: "chrome"}
	h1 := &setup.Hysteria2Outbound{Tag: "h1", Server: "203.0.113.7", ServerPort: 8443, Password: "pw"}
	first := writeConfig(t, d, []*setup.VLESSOutbound{v1, v2}, []*setup.Hysteria2Outbound{h1})
	loaded, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.VLESS) != 2 || len(loaded.Hysteria) != 1 {
		t.Fatalf("lost routes: v=%d h=%d", len(loaded.VLESS), len(loaded.Hysteria))
	}
	second, err := loaded.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if canonical(t, first) != canonical(t, second) {
		t.Fatalf("multi-route not stable")
	}
}

func TestBenchmarkProxyDiscoversManagedInbound(t *testing.T) {
	d := tempDir(t)
	v := &setup.VLESSOutbound{Tag: "route-a", Server: "203.0.113.10", ServerPort: 443, UUID: "u", SNI: "example.com", PBK: "key", FP: "chrome"}
	writeConfig(t, d, []*setup.VLESSOutbound{v}, nil)
	m, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := m.BenchmarkProxy()
	if !ok || addr != "127.0.0.1:2080" {
		t.Fatalf("managed benchmark inbound not discovered: %q, %t", addr, ok)
	}
}

func TestEnsureBenchmarkProxyMigratesLegacyManagedConfig(t *testing.T) {
	d := tempDir(t)
	legacy := []byte(`{
  "experimental": {"clash_api": {"external_controller": "127.0.0.1:9090", "secret": "fixture-secret"}},
  "outbounds": [
    {"type": "direct", "tag": "direct"},
    {"type": "block", "tag": "block"},
    {"type": "vless", "tag": "route-a", "server": "203.0.113.10", "server_port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "tls": {"enabled": true, "server_name": "example.com", "utls": {"enabled": true, "fingerprint": "chrome"}, "reality": {"enabled": true, "public_key": "fixture-key", "short_id": "abcd"}}}
  ]
}`)
	if err := os.WriteFile(filepath.Join(d, "sing-box.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := false
	runner := &SidecarRunner{
		Validator: func([]byte) error { return nil },
		Restart:   func() error { restarted = true; return nil },
		IsActive:  func() (bool, error) { return true, nil },
		ControllerReady: func(expected []string) (bool, error) {
			return len(expected) == 1 && expected[0] == "route-a", nil
		},
	}
	addr, migrated, err := EnsureBenchmarkProxy(d, "http://127.0.0.1:9090", []string{"route-a"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !migrated || addr != "127.0.0.1:2080" || !restarted {
		t.Fatalf("legacy benchmark migration failed: addr=%q migrated=%t restarted=%t", addr, migrated, restarted)
	}
	m, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := m.BenchmarkProxy(); !ok || got != addr {
		t.Fatalf("migrated listener was not persisted: %q, %t", got, ok)
	}
}

func TestEnsureBenchmarkProxyDoesNotGuessExternalController(t *testing.T) {
	d := tempDir(t)
	legacy := []byte(`{
  "experimental": {"clash_api": {"external_controller": "127.0.0.1:9090"}},
  "outbounds": [
    {"type": "direct", "tag": "direct"},
    {"type": "vless", "tag": "route-a", "server": "203.0.113.10", "server_port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "tls": {"enabled": true, "server_name": "example.com", "reality": {"enabled": true, "public_key": "fixture-key"}}}
  ]
}`)
	if err := os.WriteFile(filepath.Join(d, "sing-box.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &SidecarRunner{Restart: func() error { t.Fatal("external config must not restart"); return nil }}
	addr, migrated, err := EnsureBenchmarkProxy(d, "http://127.0.0.1:9999", []string{"route-a"}, runner)
	if err != nil || addr != "" || migrated {
		t.Fatalf("external controller was assigned a benchmark proxy: addr=%q migrated=%t err=%v", addr, migrated, err)
	}
}

func TestAddToExisting(t *testing.T) {
	d := tempDir(t)
	v1 := &setup.VLESSOutbound{Tag: "v1", Server: "203.0.113.8", ServerPort: 443, UUID: "u1", SNI: "s1", PBK: "p1", FP: "chrome"}
	first := writeConfig(t, d, []*setup.VLESSOutbound{v1}, nil)
	loaded, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	loaded.AddVLESS(&setup.VLESSOutbound{Tag: "v2", Server: "203.0.113.9", ServerPort: 443, UUID: "u2", SNI: "s2", PBK: "p2", FP: "chrome"})
	second, err := loaded.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if canonical(t, first) == canonical(t, second) {
		t.Fatalf("adding vless produced no change")
	}
	if err := os.WriteFile(filepath.Join(d, "sing-box.json"), second, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.VLESS) != 2 {
		t.Fatalf("add vless failed: %d", len(reloaded.VLESS))
	}
	reloaded.AddHysteria(&setup.Hysteria2Outbound{Tag: "h1", Server: "203.0.113.10", ServerPort: 8443, Password: "pw"})
	third, err := reloaded.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "sing-box.json"), third, 0o600); err != nil {
		t.Fatal(err)
	}
	final, _ := Load(d)
	if len(final.VLESS) != 2 || len(final.Hysteria) != 1 {
		t.Fatalf("subscription batch add failed: v=%d h=%d", len(final.VLESS), len(final.Hysteria))
	}
}

func TestRoundTripPreservesExtraConfig(t *testing.T) {
	d := tempDir(t)
	raw := []byte(`{
  "dns": {"servers": [{"tag": "custom-dns", "address": "https://example.com/dns-query"}]},
  "outbounds": [
    {"type": "direct", "tag": "direct"},
    {"type": "socks", "tag": "user-extra", "server": "203.0.113.20", "server_port": 1080},
    {"type": "vless", "tag": "managed", "server": "203.0.113.10", "server_port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "flow": "xtls-rprx-vision", "packet_encoding": "xudp", "tls": {"enabled": true, "server_name": "example.com", "utls": {"enabled": true, "fingerprint": "chrome"}, "reality": {"enabled": true, "public_key": "example-key", "short_id": "abcd", "spider_x": "/"}}}
  ]
}`)
	if err := os.WriteFile(filepath.Join(d, "sing-box.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := loaded.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(generated, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["dns"]; !ok {
		t.Fatal("non-managed top-level DNS configuration was lost")
	}
	foundExtra := false
	for _, rawOutbound := range doc["outbounds"].([]any) {
		outbound := rawOutbound.(map[string]any)
		if outbound["tag"] == "user-extra" && outbound["type"] == "socks" {
			foundExtra = true
		}
	}
	if !foundExtra {
		t.Fatal("non-managed outbound was lost")
	}
}

func TestGenerateMutatesOnlySelectorOwnedObjects(t *testing.T) {
	d := tempDir(t)
	raw := []byte(`{
  "log": {"level":"debug", "timestamp":true},
  "dns": {"servers":[{"tag":"custom-dns","address":"https://example.com/dns-query"}]},
  "experimental": {"custom_flag":{"keep":true},"cache_file":{"enabled":true,"path":"/tmp/cache.db"},"clash_api":{"external_controller":"127.0.0.1:9090","secret":"fixture-secret"}},
  "inbounds": [
    {"type":"mixed","tag":"frostgate-mixed","listen":"127.0.0.1","listen_port":23456},
    {"type":"tun","tag":"custom-inbound","mtu":1400}
  ],
  "route": {"rules":[
    {"inbound":"custom-inbound","outbound":"custom-vless"},
    {"inbound":"frostgate-mixed","outbound":"frostgate-selector"}
  ],"final":"direct"},
  "outbounds": [
    {"type":"direct","tag":"direct"},
    {"type":"selector","tag":"frostgate-selector","outbounds":["managed-vless"],"default":"managed-vless"},
    {"type":"vless","tag":"managed-vless","server":"203.0.113.10","server_port":443,"uuid":"11111111-1111-4111-8111-111111111111","tls":{"enabled":true,"server_name":"managed.example","reality":{"enabled":true,"public_key":"managed-key","short_id":"abcd"}}},
    {"type":"vless","tag":"custom-vless","server":"198.51.100.10","server_port":443,"uuid":"22222222-2222-4222-8222-222222222222","flow":"custom-flow","tls":{"enabled":true,"server_name":"custom.example","reality":{"enabled":true,"public_key":"custom-key","short_id":"cafe"}}},
    {"type":"hysteria2","tag":"custom-hysteria","server":"198.51.100.20","server_port":8443,"password":"custom-password","obfs":{"type":"salamander","password":"custom-obfs"},"tls":{"enabled":true,"server_name":"hy.example","insecure":true}}
  ]
}`)
	if err := os.WriteFile(filepath.Join(d, "sing-box.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var before map[string]any
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	m, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if m.Secret != "fixture-secret" {
		t.Fatalf("managed controller secret was not preserved: %q", m.Secret)
	}
	if len(m.VLESS) != 1 || m.VLESS[0].Tag != "managed-vless" {
		t.Fatalf("ownership detection included unrelated outbounds: %+v", m.VLESS)
	}
	if addr, ok := m.BenchmarkProxy(); !ok || addr != "127.0.0.1:23456" {
		t.Fatalf("managed listener was not retained: %q %t", addr, ok)
	}
	m.AddVLESS(&setup.VLESSOutbound{Tag: "managed-new", Server: "203.0.113.11", ServerPort: 443, UUID: "33333333-3333-4333-8333-333333333333", SNI: "new.example", PBK: "new-key", SID: "beef", FP: "chrome"})
	generated, err := m.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(generated, &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"log", "dns", "experimental"} {
		if canonicalValue(t, before[key]) != canonicalValue(t, after[key]) {
			t.Fatalf("custom top-level %s changed:\nwant %s\ngot  %s", key, canonicalValue(t, before[key]), canonicalValue(t, after[key]))
		}
	}
	for _, tag := range []string{"custom-vless", "custom-hysteria"} {
		if canonicalValue(t, outboundByTag(t, before, tag)) != canonicalValue(t, outboundByTag(t, after, tag)) {
			t.Fatalf("unowned outbound %s changed", tag)
		}
	}
	if canonicalValue(t, inboundByTag(t, before, "custom-inbound")) != canonicalValue(t, inboundByTag(t, after, "custom-inbound")) {
		t.Fatal("custom inbound changed")
	}
	if canonicalValue(t, routeRuleForInbound(t, before, "custom-inbound")) != canonicalValue(t, routeRuleForInbound(t, after, "custom-inbound")) {
		t.Fatal("custom route rule changed")
	}
	if got, ok := (&Managed{Raw: after}).BenchmarkProxy(); !ok || got != "127.0.0.1:23456" {
		t.Fatalf("generated listener changed: %q %t", got, ok)
	}
	if outboundByTag(t, after, "managed-new") == nil {
		t.Fatal("new managed outbound missing")
	}
}

func canonicalValue(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func outboundByTag(t *testing.T, doc map[string]any, tag string) map[string]any {
	t.Helper()
	for _, raw := range doc["outbounds"].([]any) {
		outbound := raw.(map[string]any)
		if outbound["tag"] == tag {
			return outbound
		}
	}
	return nil
}

func inboundByTag(t *testing.T, doc map[string]any, tag string) map[string]any {
	t.Helper()
	for _, raw := range doc["inbounds"].([]any) {
		inbound := raw.(map[string]any)
		if inbound["tag"] == tag {
			return inbound
		}
	}
	return nil
}

func routeRuleForInbound(t *testing.T, doc map[string]any, tag string) map[string]any {
	t.Helper()
	route := doc["route"].(map[string]any)
	for _, raw := range route["rules"].([]any) {
		rule := raw.(map[string]any)
		if rule["inbound"] == tag {
			return rule
		}
	}
	return nil
}

func TestSaveTransactionSuccess(t *testing.T) {
	d := tempDir(t)
	m := &Managed{Dir: d, VLESS: []*setup.VLESSOutbound{{Tag: "v1", Server: "203.0.113.11", ServerPort: 443, UUID: "u1", SNI: "s1", PBK: "p1", FP: "chrome"}}}
	var reloaded, restarted, activeCalled, readyCalled int
	runner := &SidecarRunner{
		Validator:       func([]byte) error { return nil },
		DaemonReload:    func() error { reloaded++; return nil },
		Restart:         func() error { restarted++; return nil },
		IsActive:        func() (bool, error) { activeCalled++; return true, nil },
		ControllerReady: func([]string) (bool, error) { readyCalled++; return true, nil },
	}
	if err := m.SaveTransaction(runner); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if reloaded != 1 || restarted != 1 || activeCalled != 1 || readyCalled != 1 {
		t.Fatalf("runner not fully invoked: %d %d %d %d", reloaded, restarted, activeCalled, readyCalled)
	}
	if _, err := os.Stat(filepath.Join(d, "sing-box.json")); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

func TestSaveTransactionRollbackOnRestartFail(t *testing.T) {
	d := tempDir(t)
	orig := []byte("original-config")
	if err := os.WriteFile(filepath.Join(d, "sing-box.json"), orig, 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Managed{Dir: d, VLESS: []*setup.VLESSOutbound{{Tag: "v1", Server: "203.0.113.12", ServerPort: 443, UUID: "u1", SNI: "s1", PBK: "p1", FP: "chrome"}}}
	runner := &SidecarRunner{
		DaemonReload: func() error { return nil },
		Restart:      func() error { return os.ErrPermission },
	}
	if err := m.SaveTransaction(runner); err == nil {
		t.Fatal("expected error on restart failure")
	}
	got, _ := os.ReadFile(filepath.Join(d, "sing-box.json"))
	if string(got) != string(orig) {
		t.Fatalf("rollback failed: config changed to %q", got)
	}
}

func TestSaveTransactionRollbackOnInactive(t *testing.T) {
	d := tempDir(t)
	orig := []byte("original-config")
	_ = os.WriteFile(filepath.Join(d, "sing-box.json"), orig, 0o600)
	m := &Managed{Dir: d}
	runner := &SidecarRunner{Restart: func() error { return nil }, IsActive: func() (bool, error) { return false, nil }}
	if err := m.SaveTransaction(runner); err == nil {
		t.Fatal("expected error when service inactive")
	}
	got, _ := os.ReadFile(filepath.Join(d, "sing-box.json"))
	if string(got) != string(orig) {
		t.Fatal("rollback failed on inactive")
	}
}

func TestSaveTransactionRollbackOnControllerMissing(t *testing.T) {
	d := tempDir(t)
	orig := []byte("original-config")
	_ = os.WriteFile(filepath.Join(d, "sing-box.json"), orig, 0o600)
	m := &Managed{Dir: d, VLESS: []*setup.VLESSOutbound{{Tag: "v1", Server: "203.0.113.13", ServerPort: 443, UUID: "u1", SNI: "s1", PBK: "p1", FP: "chrome"}}}
	runner := &SidecarRunner{
		Restart:         func() error { return nil },
		IsActive:        func() (bool, error) { return true, nil },
		ControllerReady: func([]string) (bool, error) { return false, nil },
	}
	if err := m.SaveTransaction(runner); err == nil {
		t.Fatal("expected error when controller missing outbound")
	}
}

func TestSaveTransactionReportsRollbackRecoveryFailures(t *testing.T) {
	d := tempDir(t)
	original := []byte(`{"outbounds":[]}`)
	if err := os.WriteFile(filepath.Join(d, "sing-box.json"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	oldWrite := atomicWriteFile
	oldRemove := removeFile
	t.Cleanup(func() { atomicWriteFile, removeFile = oldWrite, oldRemove })
	calls := 0
	atomicWriteFile = func(path string, data []byte, mode os.FileMode) error {
		calls++
		if calls >= 3 { // candidate config + secret installed; restoring config now fails.
			return errors.New("restore blocked")
		}
		return oldWrite(path, data, mode)
	}
	m := &Managed{Dir: d, Secret: "fixture-secret"}
	runner := &SidecarRunner{
		DaemonReload: func() error { return errors.New("daemon failed") },
		Restart:      func() error { return errors.New("restored restart failed") },
	}
	err := m.SaveTransaction(runner)
	if err == nil || !strings.Contains(err.Error(), "daemon failed") || !strings.Contains(err.Error(), "restore blocked") || !strings.Contains(err.Error(), "restored restart failed") {
		t.Fatalf("rollback errors were swallowed: %v", err)
	}
}

func TestSaveTransactionWithConfigRejectsInvalidCandidateBeforeMutation(t *testing.T) {
	d := tempDir(t)
	configPath := filepath.Join(d, "config.yaml")
	originalSing := []byte("original-config")
	originalConfig := []byte("endpoints: []\n")
	if err := os.WriteFile(filepath.Join(d, "sing-box.json"), originalSing, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, originalConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Managed{Dir: d}
	if err := m.SaveTransactionWithConfig(&SidecarRunner{}, configPath, []byte("endpoints: [")); err == nil {
		t.Fatal("invalid config candidate was accepted")
	}
	gotSing, _ := os.ReadFile(filepath.Join(d, "sing-box.json"))
	gotConfig, _ := os.ReadFile(configPath)
	if string(gotSing) != string(originalSing) || string(gotConfig) != string(originalConfig) {
		t.Fatal("invalid config candidate mutated files")
	}
}
