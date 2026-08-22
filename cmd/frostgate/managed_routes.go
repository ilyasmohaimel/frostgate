package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/route"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
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
	configData, controller, configuredSecret, err := routeConfigCandidate(configPath, mg.Dir, tags)
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
	if runner == nil {
		runner = managedconfig.DefaultSidecarRunner(controller, mg.Secret)
	}
	if err := mg.SaveTransactionWithConfig(runner, configPath, configData); err != nil {
		return err
	}
	return nil
}

func routeConfigCandidate(path, dir string, tags []string) ([]byte, string, string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		data, marshalErr := setup.MarshalFrostgateConfig(tags, nil, config.DefaultTestURL, config.DefaultSingBoxController, filepath.Join(dir, "controller-secret"))
		if marshalErr != nil {
			return nil, "", "", marshalErr
		}
		return data, config.DefaultSingBoxController, "", nil
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("read Frostgate config: %w", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", "", err
	}
	data, err := appendSingBoxEndpoints(raw, tags)
	if err != nil {
		return nil, "", "", err
	}
	return data, cfg.SingBox.Controller, cfg.SingBox.Secret, nil
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
