package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Fail closed even if a future test forgets to override an official endpoint.
type loopbackOnlyTransport struct{ base http.RoundTripper }

func (l loopbackOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ip := net.ParseIP(req.URL.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("test blocked non-loopback destination")
	}
	return l.base.RoundTrip(req)
}
func TestMain(m *testing.M) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	http.DefaultTransport = loopbackOnlyTransport{base}
	os.Exit(m.Run())
}

func TestClaudeAccountIdentityChangeInvalidatesFreshUsage(t *testing.T) {
	var calls atomic.Int32
	fixtureEndpoints(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"ok":true}`) })
	h := &testHost{entries: []pluginapi.HostAuthFileEntry{account("a", "claude")}, creds: map[string]string{"a": `{"access_token":"synthetic","account_uuid":"one","organization_uuid":"org-one"}`}}
	r := configuredRuntime(h)
	decodeQuota(t, r.handleManagement(quotaRequest()))
	h.creds["a"] = `{"access_token":"synthetic","account_uuid":"two","organization_uuid":"org-one"}`
	decodeQuota(t, r.handleManagement(quotaRequest()))
	h.creds["a"] = `{"access_token":"synthetic","account_uuid":"two","organization_uuid":"org-two"}`
	decodeQuota(t, r.handleManagement(quotaRequest()))
	if calls.Load() != 3 {
		t.Fatalf("account/org replacement inherited fresh usage: calls=%d", calls.Load())
	}
}

func TestReconfigureUpdatesFutureClaudeUserAgent(t *testing.T) {
	var got []string
	fixtureEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("User-Agent"))
		fmt.Fprint(w, `{}`)
	})
	h := &testHost{entries: []pluginapi.HostAuthFileEntry{account("a", "claude")}, creds: map[string]string{"a": `{"access_token":"synthetic"}`}}
	r := configuredRuntime(h)
	now := time.Now()
	r.now = func() time.Time { return now }
	decodeQuota(t, r.handleManagement(quotaRequest()))
	cfg := defaultPluginConfig()
	cfg.UserAgent = "new-agent"
	cfg.QuotaAPIKeys = []string{"test-key"}
	r.applyConfig(cfg)
	now = now.Add(time.Minute)
	decodeQuota(t, r.handleManagement(quotaRequest()))
	if len(got) != 2 || got[0] != "claude-code/2.1.80" || got[1] != "new-agent" {
		t.Fatalf("agents %v", got)
	}
}

func fixtureEndpoints(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(h)
	oldC, oldX := claudeUsageEndpoint, codexUsageEndpoint
	claudeUsageEndpoint = s.URL + "/claude"
	codexUsageEndpoint = s.URL + "/codex"
	t.Cleanup(func() { claudeUsageEndpoint = oldC; codexUsageEndpoint = oldX; s.Close() })
}

func TestProviderHeadersRawObjectsAndJWT(t *testing.T) {
	fixtureEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Errorf("bad auth/method")
		}
		switch r.URL.Path {
		case "/claude":
			if r.Header.Get("User-Agent") != "changed-agent" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || r.Header.Get("Accept") != "application/json" {
				t.Errorf("Claude headers %v", r.Header)
			}
		case "/codex":
			if r.Header.Get("Chatgpt-Account-Id") != "account-test" || r.Header.Get("Content-Type") != "application/json" || !strings.HasPrefix(r.Header.Get("User-Agent"), "codex-tui/") {
				t.Errorf("Codex headers %v", r.Header)
			}
		default:
			t.Errorf("unknown request %s", r.URL)
			http.Error(w, "no", 404)
			return
		}
		fmt.Fprint(w, `{"arbitrary":{"raw":true}}`)
	})
	jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-test"}}`)) + ".sig"
	for _, p := range []string{"claude", "codex"} {
		cfg := defaultPluginConfig()
		cfg.UserAgent = "changed-agent"
		data, category, _ := fetchUsage(context.Background(), p, credential{AccessToken: "synthetic", AccountID: "account-test"}, cfg)
		if category != "" || string(data) != `{"arbitrary":{"raw":true}}` {
			t.Fatalf("%s %s %s", p, data, category)
		}
	}
	if got := chatgptAccountIDFromJWT(jwt); got != "account-test" {
		t.Fatalf("JWT %q", got)
	}
}

func TestProviderRejectsUnsafeBodiesRedirectsAndNeverEchoesErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"null", "null", 200, "invalid_json"}, {"array", "[]", 200, "invalid_json"}, {"scalar", "42", 200, "invalid_json"}, {"malformed", "{", 200, "invalid_json"}, {"trailing", "{} {}", 200, "invalid_json"},
		{"claude oversized", `{"x":"` + strings.Repeat("x", 64<<10) + `"}`, 200, "body_too_large"},
		{"redirect", "credential-secret", 302, "http_error"}, {"error", "credential-secret", 429, "rate_limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			fixtureEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Location", "/unexpected")
				w.Header().Set("Retry-After", "999999999")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			body, category, failure := fetchUsage(context.Background(), "claude", credential{AccessToken: "synthetic"}, defaultPluginConfig())
			if category != tc.want || len(body) != 0 || requests.Load() != 1 || failure.RetryAfter > 6*time.Hour {
				t.Fatalf("body=%s category=%s failure=%+v requests=%d", body, category, failure, requests.Load())
			}
		})
	}
	t.Run("codex oversized", func(t *testing.T) {
		fixtureEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"x":"`+strings.Repeat("x", 256<<10)+`"}`)
		})
		_, category, _ := fetchUsage(context.Background(), "codex", credential{AccessToken: "synthetic"}, defaultPluginConfig())
		if category != "body_too_large" {
			t.Fatal(category)
		}
	})
}

func TestCacheIdentityTTLBackoffRecoveryAndRemoval(t *testing.T) {
	var calls atomic.Int32
	var failing atomic.Bool
	fixtureEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() {
			w.WriteHeader(503)
			fmt.Fprint(w, "secret-error-body")
			return
		}
		fmt.Fprintf(w, `{"token_marker":%q}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	})
	h := &testHost{entries: []pluginapi.HostAuthFileEntry{account("a", "claude")}, creds: map[string]string{"a": `{"access_token":"synthetic-a"}`}}
	r := configuredRuntime(h)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	view := func() accountQuota { return decodeQuota(t, r.handleManagement(quotaRequest())).Claude[0] }
	if v := view(); v.FetchedAt != "2026-01-01T00:00:00Z" || v.Error != "" {
		t.Fatalf("%+v", v)
	}
	view()
	if calls.Load() != 1 || h.gets != 2 {
		t.Fatalf("cache calls=%d gets=%d", calls.Load(), h.gets)
	}
	now = now.Add(time.Minute)
	failing.Store(true)
	v := view()
	if len(v.Usage) == 0 || v.Error == "" || v.RetryAfterMS != 300000 || strings.Contains(v.Error, "secret") {
		t.Fatalf("%+v", v)
	}
	now = now.Add(time.Minute)
	if v = view(); v.RetryAfterMS != 240000 || calls.Load() != 2 {
		t.Fatalf("%+v calls=%d", v, calls.Load())
	}
	h.creds["a"] = `{"access_token":"synthetic-b"}`
	v = view()
	if len(v.Usage) != 0 || v.RetryAfterMS != 300000 || calls.Load() != 3 {
		t.Fatalf("replacement inherited old usage/backoff: %+v calls=%d", v, calls.Load())
	}
	h.getErr = fmt.Errorf("secret-get")
	v = view()
	if len(v.Usage) != 0 || v.Email != "" || v.Label != "" || strings.Contains(v.Error, "secret") {
		t.Fatalf("auth read leaked identity: %+v", v)
	}
	h.getErr = nil
	failing.Store(false)
	now = now.Add(5 * time.Minute)
	v = view()
	if v.Error != "" || v.RetryAfterMS != 0 || !strings.Contains(string(v.Usage), "synthetic-b") {
		t.Fatalf("recovery %+v", v)
	}
	h.entries = nil
	decodeQuota(t, r.handleManagement(quotaRequest()))
	h.entries = append(h.entries, account("a", "claude"))
	before := calls.Load()
	view()
	if calls.Load() != before+1 {
		t.Fatal("deleted credential retained cache")
	}
}

func TestGlobalConcurrencyAndCoalescing(t *testing.T) {
	var active, peak, calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 32)
	fixtureEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		calls.Add(1)
		started <- struct{}{}
		<-release
		fmt.Fprint(w, `{"ok":true}`)
	})
	h := &testHost{creds: map[string]string{}}
	for i := 0; i < 8; i++ {
		id := fmt.Sprint(i)
		h.entries = append(h.entries, account(id, "codex"))
		h.creds[id] = `{"access_token":"synthetic"}`
	}
	r := configuredRuntime(h)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.handleManagement(quotaRequest()) }()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("refresh never started")
		}
	}
	close(release)
	wg.Wait()
	if peak.Load() > 4 || calls.Load() != 8 {
		t.Fatalf("peak=%d calls=%d", peak.Load(), calls.Load())
	}
}

func TestBackoffExponentialCapsAndHints(t *testing.T) {
	for _, tc := range []struct {
		n        int
		category string
		failure  usageFailure
		want     time.Duration
	}{
		{1, "server_error", usageFailure{}, 5 * time.Minute}, {2, "server_error", usageFailure{}, 10 * time.Minute}, {3, "server_error", usageFailure{}, 20 * time.Minute}, {9, "server_error", usageFailure{}, 30 * time.Minute},
		{1, "rate_limited", usageFailure{HasRetryAfter: true, RetryAfter: time.Second}, 5 * time.Minute}, {1, "server_error", usageFailure{HasRetryAfter: true, RetryAfter: 24 * time.Hour}, 6 * time.Hour},
	} {
		if got := refreshBackoffDelay(tc.n, tc.category, tc.failure); got != tc.want {
			t.Fatalf("%+v got %s", tc, got)
		}
	}
}

type orderedListHost struct {
	calls   atomic.Int32
	started chan int
	release chan struct{}
}

func (h *orderedListHost) listAuth() ([]pluginapi.HostAuthFileEntry, error) {
	n := int(h.calls.Add(1))
	h.started <- n
	if n == 1 {
		<-h.release
		return nil, nil
	}
	return []pluginapi.HostAuthFileEntry{account("new", "claude")}, nil
}
func (h *orderedListHost) getAuth(string) (json.RawMessage, error) {
	return json.RawMessage(`{"access_token":"synthetic"}`), nil
}
func TestMembershipListsSerializeBeforeReconciliation(t *testing.T) {
	fixtureEndpoints(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	h := &orderedListHost{started: make(chan int, 3), release: make(chan struct{})}
	r := configuredRuntime(h)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.handleManagement(quotaRequest()) }()
	<-h.started
	secondStarted := make(chan struct{})
	go func() { defer wg.Done(); close(secondStarted); r.handleManagement(quotaRequest()) }()
	<-secondStarted
	select {
	case n := <-h.started:
		close(h.release)
		wg.Wait()
		t.Fatalf("list %d overlapped unreconciled snapshot", n)
	case <-time.After(25 * time.Millisecond):
	}
	close(h.release)
	wg.Wait()
	r.membership.Lock()
	_, exists := r.states["claude\x00new"]
	r.membership.Unlock()
	if !exists {
		t.Fatal("new membership pruned")
	}
}

func TestProviderTimeoutAndRetryHintBounds(t *testing.T) {
	fixtureEndpoints(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	cfg := defaultPluginConfig()
	cfg.RequestTimeout = 20 * time.Millisecond
	_, category, _ := fetchUsage(context.Background(), "claude", credential{AccessToken: "synthetic"}, cfg)
	if category != "timeout" {
		t.Fatalf("%q", category)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header http.Header
		want   time.Duration
		ok     bool
	}{
		{http.Header{"Retry-After": {"60"}}, time.Minute, true},
		{http.Header{"Retry-After": {now.Add(24 * time.Hour).Format(http.TimeFormat)}}, 6 * time.Hour, true},
		{http.Header{"Retry-After": {"NaN"}}, 0, false},
		{http.Header{"Anthropic-Ratelimit-Requests-Reset": {now.Add(2 * time.Minute).Format(time.RFC3339)}}, 2 * time.Minute, true},
	} {
		got, ok := upstreamRetryDelay(tc.header, now)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("header %v got %v/%v", tc.header, got, ok)
		}
	}
}
