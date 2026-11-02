package main

import (
	"strings"
	"testing"
	"time"
)

func setQuota(t *testing.T, authID string, windows ...quotaWindow) {
	t.Helper()
	quotaMu.Lock()
	quotaSnapshots[authID] = credentialQuota{AuthID: authID, Windows: windows, FetchedAt: testNow}
	quotaMu.Unlock()
	t.Cleanup(func() {
		quotaMu.Lock()
		delete(quotaSnapshots, authID)
		quotaMu.Unlock()
	})
}

func weekly(remaining float64, resetIn time.Duration) quotaWindow {
	return quotaWindow{Name: "7d", Remaining: remaining, ResetAt: testNow.Add(resetIn), Period: weeklyWindow}
}

func spreadTestConfig(credentials ...poolCredential) pluginConfig {
	cfg := defaultPluginConfig()
	cfg.Spread.Models = []string{"claude-opus-4-8"}
	cfg.Spread.Credentials = credentials
	return cfg
}

func TestRankPoolPrefersQuotaExpiringSooner(t *testing.T) {
	// A: 30% left resetting in 24h (1.25%/h) beats B: 60% left resetting in 6 days (0.42%/h).
	setQuota(t, "a", weekly(0.30, 24*time.Hour))
	setQuota(t, "b", weekly(0.60, 144*time.Hour))
	cfg := spreadTestConfig(poolCredential{ID: "a", Provider: "claude", Weight: 1}, poolCredential{ID: "b", Provider: "antigravity", Weight: 1})
	pool := []hostAuth{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "antigravity"}}
	models := map[string][]string{"a": {"claude-opus-4-8"}, "b": {"claude-opus-4-8"}}

	ranking := rankPool(cfg, pool, models, "claude-opus-4-8", testNow)
	if ranking[0].AuthID != "a" || ranking[0].Excluded != "" {
		t.Fatalf("ranking = %+v, want a first", ranking)
	}
}

func TestRankPoolWeightScalesRate(t *testing.T) {
	setQuota(t, "pro", weekly(0.30, 24*time.Hour))
	setQuota(t, "max", weekly(0.60, 144*time.Hour))
	cfg := spreadTestConfig(poolCredential{ID: "pro", Provider: "claude", Weight: 1}, poolCredential{ID: "max", Provider: "claude", Weight: 20})
	pool := []hostAuth{{ID: "pro", Provider: "claude"}, {ID: "max", Provider: "claude"}}
	models := map[string][]string{"pro": {"claude-opus-4-8"}, "max": {"claude-opus-4-8"}}

	ranking := rankPool(cfg, pool, models, "claude-opus-4-8", testNow)
	if ranking[0].AuthID != "max" {
		t.Fatalf("ranking = %+v, want max first", ranking)
	}
}

func TestRankPoolExclusions(t *testing.T) {
	setQuota(t, "drained-5h", weekly(0.9, 48*time.Hour), quotaWindow{Name: "5h", Remaining: 0.01, ResetAt: testNow.Add(time.Hour), Period: 5 * time.Hour})
	setQuota(t, "other-model", weekly(0.9, 48*time.Hour))
	quotaMu.Lock()
	quotaSnapshots["broken"] = credentialQuota{AuthID: "broken", Error: "claude: HTTP 401", FetchedAt: testNow}
	quotaMu.Unlock()
	t.Cleanup(func() { quotaMu.Lock(); delete(quotaSnapshots, "broken"); quotaMu.Unlock() })

	cfg := spreadTestConfig()
	pool := []hostAuth{
		{ID: "drained-5h", Provider: "claude"},
		{ID: "other-model", Provider: "claude"},
		{ID: "broken", Provider: "claude"},
		{ID: "cooling", Provider: "claude", Unavailable: true, NextRetryAfter: testNow.Add(time.Minute)},
		{ID: "unfetched", Provider: "claude"},
	}
	models := map[string][]string{
		"drained-5h": {"claude-opus-4-8"}, "other-model": {"gpt-5.5"}, "broken": {"claude-opus-4-8"},
		"cooling": {"claude-opus-4-8"}, "unfetched": {"claude-opus-4-8"},
	}
	want := map[string]string{
		"drained-5h":  "5h has 1% left",
		"other-model": "does not serve",
		"broken":      "quota error",
		"cooling":     "cooling down",
		"unfetched":   "quota not fetched yet",
	}
	for _, entry := range rankPool(cfg, pool, models, "claude-opus-4-8", testNow) {
		if !strings.Contains(entry.Excluded, want[entry.AuthID]) {
			t.Fatalf("%s excluded = %q, want %q", entry.AuthID, entry.Excluded, want[entry.AuthID])
		}
	}
}

func TestRankPoolModelScopedWindow(t *testing.T) {
	// The Fable window is drained but only applies to fable models; opus ranks on the general window.
	setQuota(t, "a", weekly(0.5, 48*time.Hour), quotaWindow{Name: "7d Fable", Remaining: 0, ResetAt: testNow.Add(48 * time.Hour), Period: weeklyWindow, Models: []string{"claude-fable-5"}})
	cfg := spreadTestConfig()
	pool := []hostAuth{{ID: "a", Provider: "claude"}}
	models := map[string][]string{"a": {"claude-opus-4-8", "claude-fable-5"}}

	if entry := rankPool(cfg, pool, models, "claude-opus-4-8", testNow)[0]; entry.Excluded != "" {
		t.Fatalf("opus entry = %+v, want eligible", entry)
	}
	if entry := rankPool(cfg, pool, models, "claude-fable-5", testNow)[0]; entry.Excluded == "" {
		t.Fatalf("fable entry = %+v, want excluded", entry)
	}
}

func TestRolledWindowRefillsAfterReset(t *testing.T) {
	remaining, resetAt := rolledWindow(weekly(0, -24*time.Hour), testNow)
	if remaining != 1 || !resetAt.Equal(testNow.Add(6*24*time.Hour)) {
		t.Fatalf("rolledWindow = %v, %v; want 1, now+6d", remaining, resetAt)
	}
}

func TestSessionKey(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string][]string
		body    string
		want    string
	}{
		{"claude code header", map[string][]string{"X-Claude-Code-Session-Id": {"abc"}}, `{}`, "X-Claude-Code-Session-Id:abc"},
		{"metadata user_id", nil, `{"metadata":{"user_id":"user_x_session_y"}}`, "user_id:user_x_session_y"},
		{"prompt cache key", nil, `{"prompt_cache_key":"k"}`, "pck:k"},
		{"none", nil, `{"model":"m"}`, ""},
	}
	for _, tc := range cases {
		got, errKey := sessionKey(tc.headers, []byte(tc.body))
		if errKey != nil || got != tc.want {
			t.Fatalf("%s: sessionKey = %q, %v; want %q", tc.name, got, errKey, tc.want)
		}
	}
	if _, errKey := sessionKey(nil, []byte("not json")); errKey == nil {
		t.Fatalf("sessionKey(non-JSON) error = nil")
	}
}

func TestPickSpreadHonorsRouterChoice(t *testing.T) {
	cfg := spreadTestConfig(poolCredential{ID: "b", Provider: "claude", Weight: 1})
	req := schedulerPickRequest{
		Model:      "claude-opus-4-8",
		Options:    schedulerOptions{Metadata: map[string]any{metadataRequestedModel: "spread/claude-opus-4-8", metadataRouteAuthID: "b"}},
		Candidates: []schedulerCandidate{{ID: "a", Provider: "claude", Metadata: map[string]any{metadataQuotaSignals: nil}}, {ID: "b", Provider: "claude", Metadata: map[string]any{metadataQuotaSignals: nil}}},
	}
	resp, errPick := pick(req, cfg, testNow)
	if errPick != nil || resp.AuthID != "b" || !resp.Handled {
		t.Fatalf("pick() = %+v, %v; want router choice b", resp, errPick)
	}
}

func TestPickSpreadFailsWithoutRouterMetadata(t *testing.T) {
	cfg := spreadTestConfig(poolCredential{ID: "b", Provider: "claude", Weight: 1})
	req := schedulerPickRequest{
		Options:    schedulerOptions{Metadata: map[string]any{metadataRequestedModel: "spread/claude-opus-4-8"}},
		Candidates: []schedulerCandidate{{ID: "b", Provider: "claude", Metadata: map[string]any{metadataQuotaSignals: nil}}},
	}
	if _, errPick := pick(req, cfg, testNow); errPick == nil || !strings.Contains(errPick.Error(), metadataRouteAuthID) {
		t.Fatalf("pick() error = %v, want missing %s", errPick, metadataRouteAuthID)
	}
}

func TestValidateSpreadRequiresManagement(t *testing.T) {
	cfg := spreadTestConfig(poolCredential{ID: "a", Provider: "claude"})
	if errValidate := validateSpread(&cfg); errValidate == nil {
		t.Fatalf("validateSpread() error = nil, want management key error")
	}
	cfg.Management.Key = "k"
	if errValidate := validateSpread(&cfg); errValidate != nil || cfg.Spread.Credentials[0].Weight != 1 {
		t.Fatalf("validateSpread() = %v, weight %v; want nil, 1", errValidate, cfg.Spread.Credentials[0].Weight)
	}
}
