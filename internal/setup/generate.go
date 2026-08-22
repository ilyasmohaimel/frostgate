package setup

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/config"
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

// BuildFrostgateConfig creates the initial Frostgate config for setup flows.
// Later route additions preserve the user's existing YAML instead of replacing
// it with this initial template.
func BuildFrostgateConfig(vlessTags, hysteriaTags []string, testURL, controller, secretFile string) config.Config {
	return buildFrostgateConfig(vlessTags, hysteriaTags, testURL, controller, secretFile, true)
}

// BuildExternalControllerConfig creates a config for an unmanaged controller.
// Frostgate has not established ownership of a SOCKS listener there, so it
// must not guess benchmark_proxy.
func BuildExternalControllerConfig(tags []string, testURL, controller, secretFile string) config.Config {
	return buildFrostgateConfig(tags, nil, testURL, controller, secretFile, false)
}

// BuildDirectOnlyConfig creates a config without any proxy benchmark listener.
func BuildDirectOnlyConfig(testURL string) config.Config {
	return buildFrostgateConfig(nil, nil, testURL, config.DefaultSingBoxController, "", false)
}

func buildFrostgateConfig(vlessTags, hysteriaTags []string, testURL, controller, secretFile string, managed bool) config.Config {
	if testURL == "" {
		testURL = config.DefaultTestURL
	}
	if controller == "" {
		controller = config.DefaultSingBoxController
	}
	cfg := config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration{Duration: 5 * time.Second},
			Samples:        4,
			SampleInterval: config.Duration{Duration: 150 * time.Millisecond},
			HealthyLatency: config.Duration{Duration: 800 * time.Millisecond},
			MaxJitter:      config.Duration{Duration: 80 * time.Millisecond},
		},
		SingBox: config.SingBoxConfig{
			Controller: controller,
			SecretFile: secretFile,
			TestURL:    testURL,
		},
		Endpoints: []config.Endpoint{{
			Name:     "direct",
			Type:     config.ProbeHTTP,
			URL:      testURL,
			Priority: 100,
		}},
	}
	if managed && len(vlessTags)+len(hysteriaTags) > 0 {
		cfg.SingBox.BenchmarkProxy = "127.0.0.1:2080"
	}
	if testURL == config.DefaultTestURL {
		cfg.Endpoints[0].ExpectedStatus = 204
	}
	priority := 10
	for _, tag := range append(append([]string{}, vlessTags...), hysteriaTags...) {
		cfg.Endpoints = append(cfg.Endpoints, config.Endpoint{
			Name: tag, Type: config.ProbeSingBox, Outbound: tag, Priority: priority,
		})
		priority += 10
	}
	return cfg
}

func MarshalFrostgateConfig(vlessTags, hysteriaTags []string, testURL, controller, secretFile string) ([]byte, error) {
	return yaml.Marshal(BuildFrostgateConfig(vlessTags, hysteriaTags, testURL, controller, secretFile))
}

// GenerateFrostgateConfig is retained for callers that need the initial
// config template as text.
func GenerateFrostgateConfig(vlessTags, hysteriaTags []string, testURL, controller, secretFile string) string {
	data, err := MarshalFrostgateConfig(vlessTags, hysteriaTags, testURL, controller, secretFile)
	if err != nil {
		return ""
	}
	return string(data)
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
