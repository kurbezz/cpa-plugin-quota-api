//go:build !race

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This exercises the actual C ABI and host auth callbacks. It catches broken
// resource registration, authorization bypasses, startup fetching, and caches
// that are not shared across resource requests. No inference is sent.
func TestCLIProxyAPIQuotaAPIProcess(t *testing.T) {
	const module = "github.com/router-for-me/CLIProxyAPI/v8"
	const key = "process-quota-secret"
	var claudeHits, codexHits, metadataHits, rejectedHits, unexpectedHits atomic.Int64
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/claude":
			claudeHits.Add(1)
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-claude-token" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || r.Header.Get("User-Agent") != "claude-code/2.1.80" {
				unexpectedHits.Add(1)
				http.Error(w, "invalid Claude fixture request", 400)
				return
			}
			_, _ = io.WriteString(w, `{"five_hour":{"utilization":17,"resets_at":"2099-01-01T00:00:00Z"}}`)
		case "/codex":
			codexHits.Add(1)
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-codex-token" || r.Header.Get("Chatgpt-Account-Id") != "synthetic-codex-account" {
				unexpectedHits.Add(1)
				http.Error(w, "invalid Codex fixture request", 400)
				return
			}
			_, _ = io.WriteString(w, `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":23}}}`)
		case "/antigravity":
			metadataHits.Add(1)
			_, _ = io.WriteString(w, "version: 1.20.5\n")
		case "/xai":
			metadataHits.Add(1)
			_, _ = io.WriteString(w, `{"version":"1.0.46"}`)
		default:
			unexpectedHits.Add(1)
			http.Error(w, "unknown local fixture endpoint", 502)
		}
	}))
	t.Cleanup(fixture.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// XAI's explicit config proxy deliberately does not bypass loopback.
		// Serve only this exact linker-controlled metadata URL, never forward.
		if r.Method == http.MethodGet && r.URL.String() == fixture.URL+"/xai" {
			metadataHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"version":"1.0.46"}`)
			return
		}
		rejectedHits.Add(1)
		t.Logf("REJECTED_UPSTREAM method=%s host=%s", r.Method, r.Host)
		http.Error(w, "all non-fixture upstream traffic rejected", 502)
	}))
	t.Cleanup(proxy.Close)

	// Resolve only the pinned module, never a developer checkout or patched host.
	var resolved struct{ Dir, Version string }
	out := quotaProcessCommand(t, "", "go", "list", "-m", "-json", module+"@v8.0.58")
	if err := json.Unmarshal(out, &resolved); err != nil || resolved.Version != "v8.0.58" || resolved.Dir == "" {
		t.Fatalf("resolve stock v8.0.58 (download module dependencies first): %s; err=%v", out, err)
	}
	t.Logf("STOCK_HOST version=%s source=%s", resolved.Version, resolved.Dir)
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		t.Fatal(err)
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	quotaProcessCommand(t, "", "go", "build", "-mod=readonly", "-buildmode=c-shared", "-ldflags", "-X=main.claudeUsageEndpoint="+fixture.URL+"/claude -X=main.codexUsageEndpoint="+fixture.URL+"/codex", "-o", filepath.Join(pluginDir, "quota-api"+ext), ".")
	serverPath := filepath.Join(dir, "cliproxyapi")
	// Stock v8 starts these metadata updaters even with -local-model. Redirect
	// both before executing; the rejecting proxy is the fallback for anything else.
	quotaProcessCommand(t, resolved.Dir, "go", "build", "-mod=readonly", "-ldflags", "-X="+module+"/internal/misc.antigravityHubLatestManifestURL="+fixture.URL+"/antigravity -X="+module+"/internal/runtime/executor/helps.xaiNPMRegistryURL="+fixture.URL+"/xai", "-o", serverPath, "./cmd/server")

	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprintf("keys_configured=%t", configured), func(t *testing.T) {
			baseURL := quotaStartProcess(t, serverPath, pluginDir, proxy.URL, configured)
			if claudeHits.Load() != 0 || codexHits.Load() != 0 {
				t.Fatal("usage fetched before an authorized request")
			}
			path := baseURL + "/v0/resource/plugins/quota-api/quota"
			if !configured {
				code, _ := quotaProcessRequest(t, client, "GET", path, key)
				if code != 404 {
					t.Fatalf("unconfigured resource status=%d, want 404", code)
				}
				t.Log("OBSERVED no_keys=404 usage_hits=0")
				return
			}
			for _, bearer := range []string{"", "wrong-key"} {
				code, body := quotaProcessRequest(t, client, "GET", path, bearer)
				if code != 401 {
					t.Fatalf("unauthorized status=%d body=%s", code, body)
				}
			}
			code, body := quotaProcessRequest(t, client, "POST", path, key)
			postStatus := code
			if code < 400 || code >= 500 {
				t.Fatalf("POST not rejected: status=%d body=%s", code, body)
			}
			if claudeHits.Load() != 0 || codexHits.Load() != 0 {
				t.Fatal("unauthorized/non-GET request fetched usage")
			}
			for i := 0; i < 2; i++ {
				code, body = quotaProcessRequest(t, client, "GET", path, key)
				if code != 200 {
					t.Fatalf("quota status=%d body=%s", code, body)
				}
				quotaAssertProcessResponse(t, body)
				for _, secret := range []string{key, "synthetic-claude-token", "synthetic-codex-token", "synthetic-codex-account", "access_token", "refresh_token"} {
					if bytes.Contains(body, []byte(secret)) {
						t.Fatal("resource response disclosed credential material or key")
					}
				}
			}
			if claudeHits.Load() != 1 || codexHits.Load() != 1 {
				t.Fatalf("cache counts Claude=%d Codex=%d, want 1 each", claudeHits.Load(), codexHits.Load())
			}
			t.Logf("OBSERVED missing_key=401 wrong_key=401 post=%d authorized=200 repeated=200 claude_hits=1 codex_hits=1", postStatus)
		})
	}
	if rejectedHits.Load() != 0 || unexpectedHits.Load() != 0 {
		t.Fatalf("unexpected traffic: proxy rejected=%d fixture rejected=%d", rejectedHits.Load(), unexpectedHits.Load())
	}
	t.Logf("ISOLATION local_metadata_hits=%d rejected_upstream=0 unknown_fixture=0; no inference, live provider APIs, refresh tokens, or user config", metadataHits.Load())
	t.Log("CAPABILITIES stock /management/plugins does not expose scheduler/interceptor flags; registration unit tests own this assertion")
}

func quotaAssertProcessResponse(t *testing.T, body []byte) {
	t.Helper()
	var result struct {
		Claude []struct {
			AuthIndex string `json:"auth_index"`
			FetchedAt string `json:"fetched_at"`
			Error     string `json:"error"`
			Usage     struct {
				FiveHour struct {
					Utilization int `json:"utilization"`
				} `json:"five_hour"`
			} `json:"usage"`
		} `json:"claude"`
		Codex []struct {
			AuthIndex string `json:"auth_index"`
			FetchedAt string `json:"fetched_at"`
			Error     string `json:"error"`
			Usage     struct {
				PlanType  string `json:"plan_type"`
				RateLimit struct {
					PrimaryWindow struct {
						UsedPercent int `json:"used_percent"`
					} `json:"primary_window"`
				} `json:"rate_limit"`
			} `json:"usage"`
		} `json:"codex"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Claude) != 1 || len(result.Codex) != 1 {
		t.Fatalf("expected one physical Claude and Codex account: %s err=%v", body, err)
	}
	c, x := result.Claude[0], result.Codex[0]
	if c.AuthIndex == "" || x.AuthIndex == "" || c.Error != "" || x.Error != "" || c.Usage.FiveHour.Utilization != 17 || x.Usage.PlanType != "plus" || x.Usage.RateLimit.PrimaryWindow.UsedPercent != 23 {
		t.Fatalf("quota data mismatch: %s", body)
	}
	for _, stamp := range []string{c.FetchedAt, x.FetchedAt} {
		if _, err := time.Parse(time.RFC3339, stamp); err != nil {
			t.Fatalf("invalid fetched_at %q: %v", stamp, err)
		}
	}
	for _, field := range []string{`"protected_models"`, `"cutoff_percent_used"`, `"blocked"`} {
		if bytes.Contains(body, []byte(field)) {
			t.Fatalf("routing state in quota response: %s", body)
		}
	}
}

func quotaProcessCommand(t *testing.T, dir, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return out
}

func quotaStartProcess(t *testing.T, binary, plugins, proxy string, configured bool) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"auth", "home", "config", "cache", "data"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"claude.json": `{"type":"claude","email":"claude@example.test","access_token":"synthetic-claude-token","expired":"2099-01-01T00:00:00Z"}`,
		"codex.json":  `{"type":"codex","email":"codex@example.test","access_token":"synthetic-codex-token","account_id":"synthetic-codex-account","expired":"2099-01-01T00:00:00Z"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "auth", name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	keys := "[]"
	if configured {
		keys = `["process-quota-secret"]`
	}
	config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
proxy-url: %q
auth-dir: %q
api-keys: ["process-inference-unused"]
remote-management:
  allow-remote: false
  secret-key: "process-management-secret"
  disable-control-panel: true
  disable-auto-update-panel: true
logging-to-file: false
debug: false
plugins:
  enabled: true
  dir: %q
  configs:
    quota-api:
      enabled: true
      quota-api-keys: %s
      cache-ttl: 60s
      request-timeout: 2s
`, port, proxy, filepath.Join(dir, "auth"), plugins, keys)
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "host.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-config", configPath, "-local-model")
	cmd.Dir = dir // Do not load a developer .env or write metadata into stock source.
	cmd.Stdout, cmd.Stderr = log, log
	var env []string
	for _, item := range os.Environ() {
		name := strings.ToUpper(strings.SplitN(item, "=", 2)[0])
		// Do not inherit storage backends, Home credentials, cloud deploy mode,
		// provider credentials, or user-controlled host configuration.
		if name == "PATH" || name == "TMPDIR" || name == "TEMP" || name == "TMP" || name == "SYSTEMROOT" || name == "WINDIR" {
			env = append(env, item)
		}
	}
	cmd.Env = append(env, "HOME="+filepath.Join(dir, "home"), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_CACHE_HOME="+filepath.Join(dir, "cache"), "XDG_DATA_HOME="+filepath.Join(dir, "data"), "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy, "ALL_PROXY="+proxy, "http_proxy="+proxy, "https_proxy="+proxy, "all_proxy="+proxy, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var exit error
	go func() { exit = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("host cleanup timed out")
			}
		}
		_ = log.Close()
		out, _ := os.ReadFile(logPath)
		if bytes.Contains(out, []byte("plugin quota-api returned invalid metadata or no capabilities")) {
			t.Error("stock host rejected quota-api registration")
		}
		if t.Failed() {
			t.Logf("HOST_LOG\n%s", out)
		}
		for _, secret := range []string{"process-quota-secret", "synthetic-claude-token", "synthetic-codex-token"} {
			if bytes.Contains(out, []byte(secret)) {
				t.Error("host log disclosed secret")
			}
		}
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("host exited before ready: %v", exit)
		default:
		}
		req, _ := http.NewRequest("GET", base+"/v0/management/plugins", nil)
		req.Header.Set("Authorization", "Bearer process-management-secret")
		response, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			var listing struct {
				PluginsEnabled bool `json:"plugins_enabled"`
				Plugins        []struct {
					ID               string `json:"id"`
					Registered       bool   `json:"registered"`
					EffectiveEnabled bool   `json:"effective_enabled"`
				} `json:"plugins"`
			}
			registered := false
			if response.StatusCode == 200 && json.Unmarshal(body, &listing) == nil && listing.PluginsEnabled {
				for _, entry := range listing.Plugins {
					if entry.ID == "quota-api" && entry.Registered && entry.EffectiveEnabled {
						registered = true
					}
				}
			}
			if registered {
				authReq, _ := http.NewRequest("GET", base+"/v0/management/auth-files", nil)
				authReq.Header.Set("Authorization", "Bearer process-management-secret")
				if authResponse, authErr := client.Do(authReq); authErr == nil {
					authBody, _ := io.ReadAll(io.LimitReader(authResponse.Body, 1<<20))
					_ = authResponse.Body.Close()
					if authResponse.StatusCode == 200 && bytes.Contains(authBody, []byte("claude@example.test")) && bytes.Contains(authBody, []byte("codex@example.test")) {
						return base
					}
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("host did not register quota-api before startup deadline")
	return ""
}

func quotaProcessRequest(t *testing.T, client *http.Client, method, url, key string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}
