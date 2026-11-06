package main

import (
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
	"strings"
	"time"
)

type pluginConfig struct {
	Enabled                  bool
	CacheTTL, RequestTimeout time.Duration
	UserAgent                string
	QuotaAPIKeys             []string
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{Enabled: true, CacheTTL: time.Minute, RequestTimeout: 10 * time.Second, UserAgent: "claude-code/2.1.80"}
}
func decodeLifecycleConfig(raw []byte) (pluginConfig, error) {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &req) != nil {
		return pluginConfig{}, fmt.Errorf("invalid lifecycle request")
	}
	cfg := defaultPluginConfig()
	var decoded struct {
		Enabled        *bool    `yaml:"enabled"`
		CacheTTL       string   `yaml:"cache-ttl"`
		RequestTimeout string   `yaml:"request-timeout"`
		UserAgent      string   `yaml:"user-agent"`
		Keys           []string `yaml:"quota-api-keys"`
	}
	if yaml.Unmarshal(req.ConfigYAML, &decoded) != nil {
		return pluginConfig{}, fmt.Errorf("invalid plugin config")
	}
	if decoded.Enabled != nil {
		cfg.Enabled = *decoded.Enabled
	}
	for _, v := range []struct {
		name, value string
		target      *time.Duration
	}{{"cache-ttl", decoded.CacheTTL, &cfg.CacheTTL}, {"request-timeout", decoded.RequestTimeout, &cfg.RequestTimeout}} {
		if v.value != "" {
			d, err := time.ParseDuration(strings.TrimSpace(v.value))
			if err != nil || d <= 0 {
				return pluginConfig{}, fmt.Errorf("%s must be a positive Go duration", v.name)
			}
			*v.target = d
		}
	}
	if s := strings.TrimSpace(decoded.UserAgent); s != "" {
		cfg.UserAgent = s
	}
	for _, key := range decoded.Keys {
		if key = strings.TrimSpace(key); key != "" {
			cfg.QuotaAPIKeys = append(cfg.QuotaAPIKeys, key)
		}
	}
	return cfg, nil
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]bool    `json:"capabilities"`
}

func pluginRegistration() registration {
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: pluginapi.Metadata{Name: pluginName, Version: pluginVersion, Author: "kurbezz; based on Smarty Pants Inc quota plugin", GitHubRepository: "https://github.com/kurbezz/cpa-plugin-quota-api", ConfigFields: []pluginapi.ConfigField{
		{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable quota resource. Default true."},
		{Name: "quota-api-keys", Type: pluginapi.ConfigFieldTypeArray, Description: "Bearer keys for quota resource; empty disables the route."},
		{Name: "cache-ttl", Type: pluginapi.ConfigFieldTypeString, Description: "Positive cache duration. Default 60s."},
		{Name: "request-timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Positive provider request timeout. Default 10s."},
		{Name: "user-agent", Type: pluginapi.ConfigFieldTypeString, Description: "Claude usage User-Agent. Default claude-code/2.1.80."},
	}}, Capabilities: map[string]bool{"management_api": true}}
}

type managementRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}
type resourceRouteDef struct {
	Path string `json:"path"`
}
type managementRegistrationResponse struct {
	Routes    []managementRoute  `json:"routes"`
	Resources []resourceRouteDef `json:"resources"`
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{Routes: []managementRoute{}, Resources: []resourceRouteDef{{Path: "/quota"}}}
}
