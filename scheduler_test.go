package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func claudeCandidate(id string, resetIn time.Duration, utilization string) schedulerCandidate {
	return schedulerCandidate{ID: id, Provider: "claude", Metadata: map[string]any{
		metadataQuotaSignals: map[string]any{
			claude7dReset:       strconv.FormatInt(testNow.Add(resetIn).Unix(), 10),
			claude7dUtilization: utilization,
			claude7dStatus:      "allowed",
		},
	}}
}

func mustPick(t *testing.T, candidates ...schedulerCandidate) schedulerPickResponse {
	t.Helper()
	resp, errPick := pick(schedulerPickRequest{Candidates: candidates}, defaultPluginConfig(), testNow)
	if errPick != nil {
		t.Fatalf("pick() error = %v", errPick)
	}
	return resp
}

func TestPickPrefersEarliestWeeklyReset(t *testing.T) {
	resp := mustPick(t,
		claudeCandidate("resets-in-2-days", 48*time.Hour, "0.08"),
		claudeCandidate("resets-in-1-day", 24*time.Hour, "0.67"),
	)
	if !resp.Handled || resp.AuthID != "resets-in-1-day" {
		t.Fatalf("pick() = %+v, want resets-in-1-day", resp)
	}
}

func TestPickTieWindowPrefersMoreRemaining(t *testing.T) {
	resp := mustPick(t,
		claudeCandidate("earlier-but-drained", 24*time.Hour, "0.90"),
		claudeCandidate("slightly-later-fuller", 24*time.Hour+30*time.Minute, "0.10"),
		claudeCandidate("outside-window", 26*time.Hour, "0.00"),
	)
	if resp.AuthID != "slightly-later-fuller" {
		t.Fatalf("pick() = %+v, want slightly-later-fuller", resp)
	}
}

func TestPickSkipsExhaustedAndRejected(t *testing.T) {
	rejected5h := claudeCandidate("5h-rejected", time.Hour, "0.10")
	signals := rejected5h.Metadata[metadataQuotaSignals].(map[string]any)
	signals[claude5hStatus] = "rejected"
	signals[claude5hReset] = strconv.FormatInt(testNow.Add(2*time.Hour).Unix(), 10)

	resp := mustPick(t,
		claudeCandidate("nearly-empty", time.Hour, "0.99"),
		rejected5h,
		claudeCandidate("usable", 72*time.Hour, "0.50"),
	)
	if resp.AuthID != "usable" {
		t.Fatalf("pick() = %+v, want usable", resp)
	}
}

func TestPickRollsOverExpiredWindow(t *testing.T) {
	// Observed 8 days ago as fully used; the window has since reset and runs until 6 days from now.
	resp := mustPick(t,
		claudeCandidate("rolled-over", -24*time.Hour, "1.0"),
		claudeCandidate("resets-in-3-days", 72*time.Hour, "0.20"),
	)
	if resp.AuthID != "resets-in-3-days" {
		t.Fatalf("pick() = %+v, want resets-in-3-days", resp)
	}
	resp = mustPick(t,
		claudeCandidate("rolled-over", -24*time.Hour, "1.0"),
		claudeCandidate("resets-in-7-days", 7*24*time.Hour, "0.20"),
	)
	if resp.AuthID != "rolled-over" {
		t.Fatalf("pick() = %+v, want rolled-over", resp)
	}
}

func TestPickUnhandledWithoutWeeklySignals(t *testing.T) {
	resp := mustPick(t,
		schedulerCandidate{ID: "fresh", Provider: "claude", Metadata: map[string]any{metadataQuotaSignals: nil}},
		schedulerCandidate{ID: "codex", Provider: "codex", Metadata: map[string]any{metadataQuotaSignals: map[string]any{"X-Codex-Plan-Type": "plus"}}},
		schedulerCandidate{ID: "retry-only", Provider: "claude", Metadata: map[string]any{metadataQuotaSignals: map[string]any{"Retry-After": "30"}}},
	)
	if resp.Handled {
		t.Fatalf("pick() = %+v, want unhandled", resp)
	}
}

func TestPickFailsWhenHostOmitsQuotaMetadata(t *testing.T) {
	_, errPick := pick(schedulerPickRequest{Candidates: []schedulerCandidate{{ID: "a", Provider: "claude"}}}, defaultPluginConfig(), testNow)
	if errPick == nil || !strings.Contains(errPick.Error(), metadataQuotaSignals) {
		t.Fatalf("pick() error = %v, want missing %s error", errPick, metadataQuotaSignals)
	}
}

func TestPickFailsOnMalformedSignal(t *testing.T) {
	_, errPick := pick(schedulerPickRequest{Candidates: []schedulerCandidate{claudeCandidate("a", time.Hour, "lots")}}, defaultPluginConfig(), testNow)
	if errPick == nil || !strings.Contains(errPick.Error(), claude7dUtilization) {
		t.Fatalf("pick() error = %v, want utilization parse error", errPick)
	}
}

func TestSchedulerPickEnvelope(t *testing.T) {
	request := `{"Provider":"claude","Candidates":[` +
		`{"ID":"later","Provider":"claude","Metadata":{"quota_signals":{"Anthropic-Ratelimit-Unified-7d-Reset":"` + strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10) + `","Anthropic-Ratelimit-Unified-7d-Utilization":"0.1"}}},` +
		`{"ID":"sooner","Provider":"claude","Metadata":{"quota_signals":{"Anthropic-Ratelimit-Unified-7d-Reset":"` + strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10) + `","Anthropic-Ratelimit-Unified-7d-Utilization":"0.5"}}}]}`
	raw, errHandle := handleMethod(methodSchedulerPick, []byte(request))
	if errHandle != nil {
		t.Fatalf("handleMethod() error = %v", errHandle)
	}
	var env struct {
		OK     bool                  `json:"ok"`
		Result schedulerPickResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK || !env.Result.Handled || env.Result.AuthID != "sooner" {
		t.Fatalf("envelope = %s, want handled pick of sooner", raw)
	}
}

func TestConfigurePluginRejectsInvalidMinRemaining(t *testing.T) {
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("min_remaining: 1.5\n")})
	if errConfig := configurePlugin(raw); errConfig == nil {
		t.Fatalf("configurePlugin() error = nil, want min_remaining error")
	}
}
