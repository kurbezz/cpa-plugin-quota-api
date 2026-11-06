package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const codexUserAgent = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"

type usageFailure struct {
	StatusCode    int
	RetryAfter    time.Duration
	HasRetryAfter bool
}

func fetchUsage(ctx context.Context, provider string, c credential, cfg pluginConfig) (json.RawMessage, string, usageFailure) {
	ctx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	endpoint, limit := claudeUsageEndpoint, 64<<10
	if provider == "codex" {
		endpoint = codexUsageEndpoint
		limit = 256 << 10
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "http_error", usageFailure{}
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	if provider == "claude" {
		req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
		req.Header.Set("User-Agent", cfg.UserAgent)
	} else {
		req.Header.Set("User-Agent", codexUserAgent)
		req.Header.Set("Content-Type", "application/json")
		if c.AccountID != "" {
			req.Header.Set("Chatgpt-Account-Id", c.AccountID)
		}
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fetchErrorCategory(err, "network"), usageFailure{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		f := usageFailure{StatusCode: resp.StatusCode}
		f.RetryAfter, f.HasRetryAfter = upstreamRetryDelay(resp.Header, time.Now())
		category := "http_error"
		switch {
		case resp.StatusCode == 401:
			category = "unauthorized"
		case resp.StatusCode == 403:
			category = "forbidden"
		case resp.StatusCode == 429:
			category = "rate_limited"
		case resp.StatusCode >= 500:
			category = "server_error"
		}
		return nil, category, f
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit+1)))
	if err != nil {
		return nil, fetchErrorCategory(err, "read_error"), usageFailure{}
	}
	if len(data) > limit {
		return nil, "body_too_large", usageFailure{}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, "invalid_json", usageFailure{}
	}
	compact, err := json.Marshal(json.RawMessage(data))
	if err != nil {
		return nil, "invalid_json", usageFailure{}
	}
	return compact, "", usageFailure{}
}
func fetchErrorCategory(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return fallback
}
func chatgptAccountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var c struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return strings.TrimSpace(c.Auth.AccountID)
}

// Extracted Retry-After and Anthropic reset hint parsing, clamped before duration conversion.
func upstreamRetryDelay(header http.Header, now time.Time) (time.Duration, bool) {
	clamp := func(d time.Duration) time.Duration {
		if d < 0 {
			return 0
		}
		if d > refreshBackoffHeaderMax {
			return refreshBackoffHeaderMax
		}
		return d
	}
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if sec, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(sec) && !math.IsInf(sec, 0) && sec >= 0 {
			if sec >= refreshBackoffHeaderMax.Seconds() {
				return refreshBackoffHeaderMax, true
			}
			return time.Duration(sec * float64(time.Second)), true
		}
		if at, err := http.ParseTime(raw); err == nil {
			return clamp(at.Sub(now)), true
		}
	}
	var earliest time.Duration
	found := false
	for name, values := range header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "anthropic-ratelimit-") || !strings.HasSuffix(lower, "-reset") || len(values) == 0 {
			continue
		}
		raw := strings.TrimSpace(values[0])
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			sec, e := strconv.ParseFloat(raw, 64)
			if e != nil || math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 || sec > float64(1<<62) {
				continue
			}
			if sec >= float64(now.Unix())+refreshBackoffHeaderMax.Seconds() {
				at = now.Add(refreshBackoffHeaderMax)
			} else {
				at = time.Unix(int64(sec), 0)
			}
		}
		if !at.After(now) {
			continue
		}
		d := clamp(at.Sub(now))
		if !found || d < earliest {
			earliest = d
			found = true
		}
	}
	return earliest, found
}
