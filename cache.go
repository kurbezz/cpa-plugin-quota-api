package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"strings"
	"sync"
	"time"
)

const (
	refreshBackoffBase      = 5 * time.Minute
	refreshBackoffMax       = 30 * time.Minute
	refreshBackoffHeaderMax = 6 * time.Hour
)

type accountState struct {
	mu        sync.Mutex
	identity  [32]byte
	usage     json.RawMessage
	fetchedAt time.Time
	failures  int
	nextAt    time.Time
	lastErr   string
}
type credential struct {
	Type             string `json:"type"`
	AccessToken      string `json:"access_token"`
	AccountID        string `json:"account_id"`
	IDToken          string `json:"id_token"`
	AccountUUID      string `json:"account_uuid"`
	OrganizationID   string `json:"organization_id"`
	OrganizationUUID string `json:"organization_uuid"`
}

func (r *pluginRuntime) accountQuota(e pluginapi.HostAuthFileEntry, provider string, s *accountState, cfg pluginConfig) accountQuota {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Always resolve the current credential before inspecting a cached value or pause.
	raw, err := r.host.getAuth(e.AuthIndex)
	var c credential
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return accountQuota{AuthIndex: e.AuthIndex, Error: "auth_get"}
	}
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.AccountID = strings.TrimSpace(c.AccountID)
	if c.AccessToken == "" {
		return accountQuota{AuthIndex: e.AuthIndex, Error: "missing_token"}
	}
	if c.Type != "" && !strings.EqualFold(strings.TrimSpace(c.Type), provider) {
		return accountQuota{AuthIndex: e.AuthIndex, Error: "invalid_credential"}
	}
	if provider == "codex" && c.AccountID == "" {
		c.AccountID = chatgptAccountIDFromJWT(c.IDToken)
	}
	identityBytes, _ := json.Marshal([]string{provider, c.AccessToken, c.AccountID, c.IDToken, strings.TrimSpace(c.AccountUUID), strings.TrimSpace(c.OrganizationID), strings.TrimSpace(c.OrganizationUUID)})
	identity := sha256.Sum256(identityBytes)
	if s.identity != identity {
		s.identity = identity
		s.usage = nil
		s.fetchedAt = time.Time{}
		s.failures = 0
		s.nextAt = time.Time{}
		s.lastErr = ""
	}
	now := r.now()
	if now.Before(s.nextAt) || (s.lastErr == "" && len(s.usage) > 0 && now.Sub(s.fetchedAt) < cfg.CacheTTL) {
		return accountView(e, s, now)
	}
	r.sem <- struct{}{}
	body, category, failure := fetchUsage(context.Background(), provider, c, cfg)
	<-r.sem
	now = r.now()
	if category != "" {
		s.failures++
		delay := refreshBackoffDelay(s.failures, category, failure)
		if r.jitter != nil {
			j := r.jitter(delay)
			if j < 0 {
				j = 0
			}
			if j > delay/10 {
				j = delay / 10
			}
			delay += j
		}
		cap := refreshBackoffMax
		if failure.HasRetryAfter {
			cap = refreshBackoffHeaderMax
		}
		if delay > cap {
			delay = cap
		}
		s.nextAt = now.Add(delay)
		s.lastErr = category
	} else {
		s.usage = body
		s.fetchedAt = now
		s.failures = 0
		s.nextAt = time.Time{}
		s.lastErr = ""
	}
	return accountView(e, s, now)
}
func accountView(e pluginapi.HostAuthFileEntry, s *accountState, now time.Time) accountQuota {
	v := accountQuota{AuthIndex: e.AuthIndex, Email: e.Email, Label: e.Label, Usage: s.usage, Error: s.lastErr}
	if len(s.usage) > 0 {
		v.FetchedAt = s.fetchedAt.UTC().Format(time.RFC3339)
	}
	if s.lastErr != "" && now.Before(s.nextAt) {
		v.RetryAfterMS = s.nextAt.Sub(now).Milliseconds()
	}
	return v
}

// Extracted from the original plugin's bounded failure schedule.
func refreshBackoffDelay(failures int, category string, failure usageFailure) time.Duration {
	delay := refreshBackoffBase
	for i := 1; i < failures && delay < refreshBackoffMax; i++ {
		delay *= 2
	}
	if delay > refreshBackoffMax {
		delay = refreshBackoffMax
	}
	if failure.HasRetryAfter {
		delay = failure.RetryAfter
		if delay > refreshBackoffHeaderMax {
			delay = refreshBackoffHeaderMax
		}
	}
	if (category == "rate_limited" || failure.StatusCode == 429) && delay < refreshBackoffBase {
		delay = refreshBackoffBase
	}
	if delay < 0 {
		delay = 0
	}
	return delay
}
