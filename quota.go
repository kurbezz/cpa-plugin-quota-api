package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

type hostClient interface {
	listAuth() ([]pluginapi.HostAuthFileEntry, error)
	getAuth(string) (json.RawMessage, error)
}
type pluginRuntime struct {
	host    hostClient
	mu      sync.RWMutex
	cfg     pluginConfig
	stopped bool
	// Serialize list completion and reconciliation: an older snapshot cannot prune newer membership.
	membership sync.Mutex
	states     map[string]*accountState
	sem        chan struct{}
	now        func() time.Time
	jitter     func(time.Duration) time.Duration
}

func newPluginRuntime(h hostClient) *pluginRuntime {
	return &pluginRuntime{host: h, cfg: defaultPluginConfig(), states: map[string]*accountState{}, sem: make(chan struct{}, 4), now: time.Now, jitter: func(d time.Duration) time.Duration {
		if d <= 0 {
			return 0
		}
		return time.Duration(rand.Int64N(int64(d/10) + 1))
	}}
}
func (r *pluginRuntime) applyConfig(cfg pluginConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	r.stopped = false
}
func (r *pluginRuntime) shutdown() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.membership.Lock()
	r.states = map[string]*accountState{}
	r.membership.Unlock()
}

type accountQuota struct {
	AuthIndex    string          `json:"auth_index"`
	Email        string          `json:"email,omitempty"`
	Label        string          `json:"label,omitempty"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	FetchedAt    string          `json:"fetched_at,omitempty"`
	Error        string          `json:"error,omitempty"`
	RetryAfterMS int64           `json:"retry_after_ms,omitempty"`
}
type quotaResponse struct {
	Claude []accountQuota `json:"claude"`
	Codex  []accountQuota `json:"codex"`
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	b, _ := json.Marshal(v)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, Body: b}
}
func quotaKeyAuthorized(headers http.Header, keys []string) bool {
	var value string
	for k, v := range headers {
		if strings.EqualFold(k, "Authorization") && len(v) > 0 {
			value = strings.TrimSpace(v[0])
			break
		}
	}
	if len(value) < 7 || !strings.EqualFold(value[:7], "bearer ") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(value[7:])))
	match := 0
	for _, k := range keys {
		want := sha256.Sum256([]byte(k))
		match |= subtle.ConstantTimeCompare(got[:], want[:])
	}
	return match == 1
}
func providerFor(e pluginapi.HostAuthFileEntry) string {
	p := strings.ToLower(strings.TrimSpace(e.Provider))
	if p == "" {
		p = strings.ToLower(strings.TrimSpace(e.Type))
	}
	if p != "claude" && p != "codex" {
		return ""
	}
	if e.Disabled || strings.EqualFold(strings.TrimSpace(e.Status), "disabled") || e.RuntimeOnly || strings.TrimSpace(e.Path) == "" || e.AuthIndex == "" {
		return ""
	}
	return p
}
func (r *pluginRuntime) handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	r.mu.RLock()
	cfg, stopped := r.cfg, r.stopped
	r.mu.RUnlock()
	fail := func(status int, msg string) pluginapi.ManagementResponse {
		return jsonResponse(status, map[string]string{"error": msg})
	}
	if (req.Path != "/quota" && req.Path != "/v0/resource/plugins/quota-api/quota") || !cfg.Enabled || stopped || len(cfg.QuotaAPIKeys) == 0 {
		return fail(404, "not found")
	}
	if !quotaKeyAuthorized(req.Headers, cfg.QuotaAPIKeys) {
		return fail(401, "unauthorized")
	}
	if req.Method != http.MethodGet {
		return fail(405, "method not allowed")
	}
	r.membership.Lock()
	entries, err := r.host.listAuth()
	if err != nil {
		r.membership.Unlock()
		return fail(502, "credential list unavailable")
	}
	type work struct {
		entry    pluginapi.HostAuthFileEntry
		provider string
		state    *accountState
	}
	jobs := []work{}
	keep := map[string]bool{}
	for _, e := range entries {
		p := providerFor(e)
		if p == "" {
			continue
		}
		key := p + "\x00" + e.AuthIndex
		if keep[key] {
			continue
		}
		keep[key] = true
		s := r.states[key]
		if s == nil {
			s = &accountState{}
			r.states[key] = s
		}
		jobs = append(jobs, work{e, p, s})
	}
	for key := range r.states {
		if !keep[key] {
			delete(r.states, key)
		}
	}
	r.membership.Unlock()
	views := make([]accountQuota, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func() { defer wg.Done(); views[i] = r.accountQuota(job.entry, job.provider, job.state, cfg) }()
	}
	wg.Wait()
	out := quotaResponse{Claude: []accountQuota{}, Codex: []accountQuota{}}
	for i, v := range views {
		if jobs[i].provider == "claude" {
			out.Claude = append(out.Claude, v)
		} else {
			out.Codex = append(out.Codex, v)
		}
	}
	return jsonResponse(200, out)
}
