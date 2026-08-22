package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/profile"
	"github.com/ilyasmohaimel/frostgate/internal/route"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

var (
	findSingBox      = exec.LookPath
	writeSystemdUnit = setup.WriteSystemdUnit
	newSidecarRunner = managedconfig.DefaultSidecarRunnerForService
)

type managedRouteResult struct {
	Tag           string
	AlreadyExists bool
	ConfigPath    string
}

// addManagedRoute is the single route-add path used by the CLI and setup. It
// parses with route.ParseShareLink, updates the managed sing-box model, and
// installs sing-box.json and config.yaml in one transaction.
func addManagedRoute(dir, configPath, link, tag string, runner *managedconfig.SidecarRunner) (managedRouteResult, error) {
	if err := route.ValidateName(tag); err != nil {
		return managedRouteResult{}, fmt.Errorf("invalid route name %q: %w", tag, err)
	}
	r, err := route.ParseShareLink(link, tag)
	if err != nil {
		return managedRouteResult{}, err
	}
	mg, err := managedconfig.Load(dir)
	if err != nil {
		return managedRouteResult{}, fmt.Errorf("load managed config: %w", err)
	}
	for _, v := range mg.VLESS {
		if v.Tag == tag || route.FingerprintVLESS(v) == r.Fingerprint {
			return managedRouteResult{Tag: v.Tag, AlreadyExists: true, ConfigPath: configPath}, nil
		}
	}
	for _, h := range mg.Hysteria {
		if h.Tag == tag || route.FingerprintHysteria(h) == r.Fingerprint {
			return managedRouteResult{Tag: h.Tag, AlreadyExists: true, ConfigPath: configPath}, nil
		}
	}
	if r.VLESS != nil {
		mg.AddVLESS(r.VLESS)
	} else {
		mg.AddHysteria(r.Hysteria)
	}

	if err := commitManagedRoutes(mg, configPath, []string{tag}, runner); err != nil {
		return managedRouteResult{}, err
	}
	return managedRouteResult{Tag: tag, ConfigPath: configPath}, nil
}

func commitManagedRoutes(mg *managedconfig.Managed, configPath string, tags []string, runner *managedconfig.SidecarRunner) error {
	identity := profileForPaths(configPath, mg.Dir)
	newSidecar := !mg.HasManagedSidecar()
	needsProfileBindings := newSidecar || (identity.ID != profile.DefaultID && !mg.UsesProfileBindings(identity.ControllerAddr, identity.BenchmarkAddr))
	if identity.ID != profile.DefaultID && needsProfileBindings {
		if err := identity.CheckAvailable(); err != nil {
			return err
		}
	}
	configData, controller, configuredSecret, err := routeConfigCandidate(configPath, mg.Dir, tags, identity, newSidecar)
	if err != nil {
		return err
	}
	if mg.Secret == "" {
		mg.Secret = configuredSecret
		if mg.Secret == "" {
			mg.Secret, err = setup.GenerateSecret()
			if err != nil {
				return err
			}
		}
	}
	// A custom profile that owns a sidecar gets unique loopback bindings. The
	// default profile keeps its long-standing ports and unit name.
	if newSidecar || identity.ID != profile.DefaultID {
		mg.ConfigureProfile(controller, identity.BenchmarkAddr)
	}
	var restoreUnit func() error
	if runner == nil {
		if newSidecar || identity.ID != profile.DefaultID {
			sbPath, lookErr := findSingBox("sing-box")
			if lookErr != nil {
				return fmt.Errorf("sing-box not available for managed sidecar bootstrap: %w", lookErr)
			}
			var unitErr error
			restoreUnit, unitErr = writeManagedUnit(setup.ServiceUnitPath(identity.Service), sbPath, mg.Dir)
			if unitErr != nil {
				return unitErr
			}
		}
		runner = newSidecarRunner(controller, mg.Secret, identity.Service)
	}
	if err := mg.SaveTransactionWithConfig(runner, configPath, configData); err != nil {
		if restoreUnit != nil {
			var recovery error
			if restoreErr := restoreUnit(); restoreErr != nil {
				recovery = errors.Join(recovery, fmt.Errorf("restore service unit: %w", restoreErr))
			}
			if runner != nil && runner.DaemonReload != nil {
				if reloadErr := runner.DaemonReload(); reloadErr != nil {
					recovery = errors.Join(recovery, fmt.Errorf("reload restored service unit: %w", reloadErr))
				}
			}
			if recovery != nil {
				return errors.Join(err, recovery)
			}
		}
		return err
	}
	return nil
}

// writeManagedUnit snapshots a profile unit so failed first-route bootstraps
// do not leave a sidecar registered for a configuration that was rolled back.
func writeManagedUnit(path, binaryPath, configDir string) (func() error, error) {
	before, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("snapshot service unit: %w", err)
	}
	if err := writeSystemdUnit(path, binaryPath, configDir); err != nil {
		return nil, err
	}
	return func() error {
		if existed {
			if err := os.WriteFile(path, before, 0o644); err != nil {
				return err
			}
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}, nil
}

func routeConfigCandidate(path, dir string, tags []string, identity profile.Identity, bootstrap bool) ([]byte, string, string, error) {
	controller := "http://" + identity.ControllerAddr
	secretFile := filepath.Join(dir, "controller-secret")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		data, marshalErr := setup.MarshalFrostgateConfig(tags, nil, config.DefaultTestURL, controller, secretFile)
		if marshalErr != nil {
			return nil, "", "", marshalErr
		}
		data, marshalErr = setManagedSingBoxSettings(data, controller, secretFile, identity.BenchmarkAddr)
		if marshalErr != nil {
			return nil, "", "", marshalErr
		}
		return data, controller, "", nil
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("read Frostgate config: %w", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", "", err
	}
	if bootstrap {
		for _, endpoint := range cfg.Endpoints {
			if endpoint.Type == config.ProbeSingBox {
				return nil, "", "", fmt.Errorf("cannot bootstrap a managed sidecar over an existing external controller")
			}
		}
	}
	data, err := appendSingBoxEndpoints(raw, tags)
	if err != nil {
		return nil, "", "", err
	}
	if bootstrap || identity.ID != profile.DefaultID {
		data, err = setManagedSingBoxSettings(data, controller, secretFile, identity.BenchmarkAddr)
		if err != nil {
			return nil, "", "", err
		}
		return data, controller, cfg.SingBox.Secret, nil
	}
	return data, cfg.SingBox.Controller, cfg.SingBox.Secret, nil
}

func setManagedSingBoxSettings(raw []byte, controller, secretFile, benchmarkAddr string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse Frostgate config for managed sidecar: %w", err)
	}
	root := doc.Content[0]
	singbox := mappingValue(root, "singbox")
	if singbox == nil {
		singbox = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "singbox"}, singbox)
	}
	if singbox.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("Frostgate config singbox must be a mapping")
	}
	setMappingString(singbox, "controller", controller)
	setMappingString(singbox, "secret_file", secretFile)
	setMappingString(singbox, "benchmark_proxy", benchmarkAddr)
	return yaml.Marshal(&doc)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func setMappingString(mapping *yaml.Node, key, value string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Kind = yaml.ScalarNode
			mapping.Content[i+1].Tag = "!!str"
			mapping.Content[i+1].Value = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func appendSingBoxEndpoints(raw []byte, tags []string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse Frostgate config: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("Frostgate config must be a mapping")
	}
	root := doc.Content[0]
	var endpoints *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "endpoints" {
			endpoints = root.Content[i+1]
			break
		}
	}
	if endpoints == nil {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "endpoints"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"},
		)
		endpoints = root.Content[len(root.Content)-1]
	}
	if endpoints.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("Frostgate config endpoints must be a list")
	}
	maxPriority := 0
	for _, endpoint := range endpoints.Content {
		if endpoint.Kind != yaml.MappingNode || endpointField(endpoint, "type") != string(config.ProbeSingBox) {
			continue
		}
		if priority, err := strconv.Atoi(endpointField(endpoint, "priority")); err == nil && priority > maxPriority {
			maxPriority = priority
		}
	}
	nextPriority := ((maxPriority / 10) + 1) * 10
	for _, tag := range tags {
		for _, endpoint := range endpoints.Content {
			if endpoint.Kind != yaml.MappingNode {
				continue
			}
			for i := 0; i+1 < len(endpoint.Content); i += 2 {
				if endpoint.Content[i].Value == "name" && endpoint.Content[i+1].Value == tag {
					return nil, fmt.Errorf("endpoint %q already exists", tag)
				}
			}
		}
		endpointData, err := yaml.Marshal(config.Endpoint{Name: tag, Type: config.ProbeSingBox, Outbound: tag, Priority: nextPriority})
		if err != nil {
			return nil, err
		}
		var endpoint yaml.Node
		if err := yaml.Unmarshal(endpointData, &endpoint); err != nil || len(endpoint.Content) != 1 {
			return nil, fmt.Errorf("encode endpoint: %w", err)
		}
		endpoints.Content = append(endpoints.Content, endpoint.Content[0])
		nextPriority += 10
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("encode Frostgate config: %w", err)
	}
	return data, nil
}

func endpointField(endpoint *yaml.Node, key string) string {
	for i := 0; i+1 < len(endpoint.Content); i += 2 {
		if endpoint.Content[i].Value == key {
			return endpoint.Content[i+1].Value
		}
	}
	return ""
}
