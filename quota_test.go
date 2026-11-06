package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type testHost struct {
	mu              sync.Mutex
	entries         []pluginapi.HostAuthFileEntry
	creds           map[string]string
	listErr, getErr error
	lists, gets     int
}

func (h *testHost) listAuth() ([]pluginapi.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lists++
	return append([]pluginapi.HostAuthFileEntry(nil), h.entries...), h.listErr
}
func (h *testHost) getAuth(index string) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gets++
	return json.RawMessage(h.creds[index]), h.getErr
}
func quotaRequest() pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Method: "GET", Path: "/quota", Headers: http.Header{"Authorization": {"Bearer test-key"}}}
}
func configuredRuntime(h hostClient) *pluginRuntime {
	r := newPluginRuntime(h)
	cfg := defaultPluginConfig()
	cfg.QuotaAPIKeys = []string{"test-key"}
	r.applyConfig(cfg)
	r.jitter = func(time.Duration) time.Duration { return 0 }
	return r
}
func account(index, provider string) pluginapi.HostAuthFileEntry {
	return pluginapi.HostAuthFileEntry{AuthIndex: index, Provider: provider, Path: "/synthetic/" + index + ".json", Email: index + "@example.invalid", Label: index}
}
func decodeQuota(t *testing.T, r pluginapi.ManagementResponse) quotaResponse {
	t.Helper()
	if r.StatusCode != 200 {
		t.Fatalf("status %d: %s", r.StatusCode, r.Body)
	}
	var v quotaResponse
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestResourceAuthorizationNeverTouchesHost(t *testing.T) {
	for _, tc := range []struct {
		name, method, key, path string
		enabled                 bool
		keys                    []string
		want                    int
	}{
		{"no keys", "GET", "test-key", "/quota", true, nil, 404},
		{"missing", "GET", "", "/quota", true, []string{"test-key"}, 401},
		{"wrong", "GET", "wrong", "/quota", true, []string{"test-key"}, 401},
		{"disabled", "GET", "test-key", "/quota", false, []string{"test-key"}, 404},
		{"method", "POST", "test-key", "/quota", true, []string{"test-key"}, 405},
		{"path", "GET", "test-key", "/other", true, []string{"test-key"}, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &testHost{}
			r := newPluginRuntime(h)
			cfg := defaultPluginConfig()
			cfg.Enabled = tc.enabled
			cfg.QuotaAPIKeys = tc.keys
			r.applyConfig(cfg)
			req := quotaRequest()
			req.Method = tc.method
			req.Path = tc.path
			req.Headers.Set("Authorization", "Bearer "+tc.key)
			got := r.handleManagement(req)
			if got.StatusCode != tc.want || h.lists != 0 || h.gets != 0 {
				t.Fatalf("status=%d calls=%d/%d", got.StatusCode, h.lists, h.gets)
			}
		})
	}
}

func TestResourceEmptyArraysAndSafeListFailure(t *testing.T) {
	h := &testHost{}
	r := configuredRuntime(h)
	resp := r.handleManagement(quotaRequest())
	if string(resp.Body) != "{\"claude\":[],\"codex\":[]}" {
		t.Fatalf("%s", resp.Body)
	}
	h.listErr = errors.New("private-secret")
	resp = r.handleManagement(quotaRequest())
	if resp.StatusCode != 502 || strings.Contains(string(resp.Body), "private-secret") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Body)
	}
}

func TestResourceFiltersNonPhysicalAndUnsupportedCredentials(t *testing.T) {
	h := &testHost{creds: map[string]string{}}
	for _, e := range []pluginapi.HostAuthFileEntry{account("disabled", "claude"), account("virtual", "codex"), account("memory", "claude"), account("other", "gemini"), account("status", "codex")} {
		switch e.AuthIndex {
		case "disabled":
			e.Disabled = true
		case "virtual":
			e.RuntimeOnly = true
		case "memory":
			e.Path = ""
		case "status":
			e.Status = "disabled"
		}
		h.entries = append(h.entries, e)
	}
	v := decodeQuota(t, configuredRuntime(h).handleManagement(quotaRequest()))
	if len(v.Claude) != 0 || len(v.Codex) != 0 || h.gets != 0 {
		t.Fatalf("unexpected accounts: %+v gets=%d", v, h.gets)
	}
}

func TestLifecycleRegistersOnlyResourceAndRejectsInvalidDurations(t *testing.T) {
	raw, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Metadata     pluginapi.Metadata `json:"metadata"`
			Capabilities map[string]bool    `json:"capabilities"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatalf("%s", raw)
	}
	if env.Result.Metadata.Name != "quota-api" || env.Result.Metadata.Version != "0.1.0" {
		t.Fatalf("%s", raw)
	}
	for k, v := range env.Result.Capabilities {
		if v != (k == "management_api") {
			t.Fatalf("unexpected capability %s=%v", k, v)
		}
	}
	reg := managementRegistration()
	if len(reg.Routes) != 0 || len(reg.Resources) != 1 || reg.Resources[0].Path != "/quota" {
		t.Fatalf("%+v", reg)
	}
	for _, y := range []string{"cache-ttl: 0s", "cache-ttl: -1s", "request-timeout: 0s", "request-timeout: garbage"} {
		b, _ := json.Marshal(map[string][]byte{"config_yaml": []byte(y)})
		if _, err := decodeLifecycleConfig(b); err == nil {
			t.Errorf("accepted %s", y)
		}
	}
	cfg, err := decodeLifecycleConfig(nil)
	if err != nil || !cfg.Enabled || cfg.CacheTTL != 60*time.Second || cfg.RequestTimeout != 10*time.Second || cfg.UserAgent != "claude-code/2.1.80" || len(cfg.QuotaAPIKeys) != 0 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if raw, err := handleMethod("scheduler.pick", nil); err != nil || !strings.Contains(string(raw), "unknown_method") {
		t.Fatalf("%s %v", raw, err)
	}
}

func TestLifecycleSuppliesMetadataRequiredByStockHost(t *testing.T) {
	// Stock v8 host.go validPlugin rejects any of these four blank fields.
	raw, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Metadata pluginapi.Metadata `json:"metadata"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("registration %s err=%v", raw, err)
	}
	m := env.Result.Metadata
	for name, value := range map[string]string{"Name": m.Name, "Version": m.Version, "Author": m.Author, "GitHubRepository": m.GitHubRepository} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("stock host rejects blank required metadata %s", name)
		}
	}
}
