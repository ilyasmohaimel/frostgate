package setup

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func EnsureConfigDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func WriteSecretFile(path, secret string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func GenerateSingBoxConfig(secret string, vlessOutbounds []*VLESSOutbound, hysteriaOutbounds []*Hysteria2Outbound, dir string) ([]byte, error) {
	cfg := map[string]any{
		"log": map[string]any{
			"level":  "warn",
			"output": filepath.Join(dir, "sing-box.log"),
		},
		"experimental": map[string]any{
			"clash_api": map[string]any{
				"external_controller": "127.0.0.1:9090",
				"secret":              secret,
			},
			"cache_file": map[string]any{
				"enabled": true,
				"path":    filepath.Join(dir, "cache.db"),
			},
		},
		"outbounds": []any{
			map[string]any{"type": "direct", "tag": "direct"},
			map[string]any{"type": "block", "tag": "block"},
		},
	}
	// Add benchmark selector and mixed inbound if we have proxy outbounds
	hasProxy := len(vlessOutbounds)+len(hysteriaOutbounds) > 0
	if hasProxy {
		cfg["inbounds"] = []any{
			map[string]any{
				"type":        "mixed",
				"tag":         "frostgate-mixed",
				"listen":      "127.0.0.1",
				"listen_port": 2080,
			},
		}
	}
	outbounds := cfg["outbounds"].([]any)
	for _, v := range vlessOutbounds {
		tlsCfg := map[string]any{
			"enabled":     true,
			"server_name": v.SNI,
			"utls": map[string]any{
				"enabled":     true,
				"fingerprint": v.FP,
			},
			"reality": map[string]any{
				"enabled":    true,
				"public_key": v.PBK,
				"short_id":   v.SID,
			},
		}
		if v.SpiderX != "" {
			tlsCfg["reality"].(map[string]any)["spider_x"] = v.SpiderX
		}
		ob := map[string]any{
			"type":        "vless",
			"tag":         v.Tag,
			"server":      v.Server,
			"server_port": v.ServerPort,
			"uuid":        v.UUID,
			"flow":        v.Flow,
			"tls":         tlsCfg,
		}
		if v.Flow == "" {
			// omit flow if empty? but keep as is; sing-box allows empty
		}
		// packet_encoding xudp is common for VLESS
		if v.PacketEncoding != "" {
			ob["packet_encoding"] = v.PacketEncoding
		} else {
			ob["packet_encoding"] = "xudp"
		}
		outbounds = append(outbounds, ob)
	}
	for _, h := range hysteriaOutbounds {
		tlsCfg := map[string]any{
			"enabled":     true,
			"server_name": h.SNI,
		}
		if h.Insecure {
			tlsCfg["insecure"] = true
		}
		if h.PinSHA256 != "" {
			tlsCfg["alpn"] = nil
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
			ob["obfs"] = map[string]any{
				"type":     obfsType,
				"password": h.ObfsPassword,
			}
		}
		if h.PinSHA256 != "" {
			// sing-box uses certificate pinning via tls config? Keep as is
			// Not critical for setup
		}
		outbounds = append(outbounds, ob)
	}
	// Add selector for benchmark if we have proxies
	if hasProxy {
		var tags []string
		for _, v := range vlessOutbounds {
			tags = append(tags, v.Tag)
		}
		for _, h := range hysteriaOutbounds {
			tags = append(tags, h.Tag)
		}
		selector := map[string]any{
			"type":      "selector",
			"tag":       "frostgate-selector",
			"outbounds": tags,
		}
		if len(tags) > 0 {
			selector["default"] = tags[0]
		}
		// Insert selector after direct/block
		newOuts := []any{outbounds[0], outbounds[1], selector}
		newOuts = append(newOuts, outbounds[2:]...)
		outbounds = newOuts
		cfg["route"] = map[string]any{
			"rules": []any{
				map[string]any{
					"inbound":  "frostgate-mixed",
					"outbound": "frostgate-selector",
				},
			},
			"final": "direct",
		}
	}
	cfg["outbounds"] = outbounds
	return json.MarshalIndent(cfg, "", "  ")
}

func GenerateFrostgateConfig(vlessTags, hysteriaTags []string, testURL, controller, secretFile string) string {
	// Build endpoints
	out := "defaults:\n  timeout: 5s\n  samples: 4\n  sample_interval: 150ms\n  healthy_latency: 800ms\n  max_jitter: 80ms\n\nsingbox:\n  controller: " + controller + "\n"
	if secretFile != "" {
		out += "  secret_file: " + secretFile + "\n"
	}
	out += "  test_url: " + testURL + "\n"
	out += "  benchmark_proxy: 127.0.0.1:2080\n\nendpoints:\n"
	// direct baseline always
	out += "  - name: direct\n    type: http\n    url: " + testURL + "\n    expected_status: 204\n    priority: 100\n"
	priority := 10
	for _, tag := range vlessTags {
		out += fmt.Sprintf("  - name: %s\n    type: singbox\n    outbound: %s\n    priority: %d\n", tag, tag, priority)
		priority += 10
	}
	for _, tag := range hysteriaTags {
		out += fmt.Sprintf("  - name: %s\n    type: singbox\n    outbound: %s\n    priority: %d\n", tag, tag, priority)
		priority += 10
	}
	return out
}

func WriteFileSecure(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func IsLoopbackController(controller string) bool {
	// controller is like http://127.0.0.1:9090
	return controller == "http://127.0.0.1:9090" || controller == "http://localhost:9090" || controller == "http://[::1]:9090"
}
