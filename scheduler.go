package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// Keys of SchedulerAuthCandidate.Metadata populated by the host (pluginapi.SchedulerQuota*MetadataKey).
	metadataQuotaSignals = "quota_signals"

	claude7dReset       = "Anthropic-Ratelimit-Unified-7d-Reset"
	claude7dUtilization = "Anthropic-Ratelimit-Unified-7d-Utilization"
	claude7dStatus      = "Anthropic-Ratelimit-Unified-7d-Status"
	claude5hReset       = "Anthropic-Ratelimit-Unified-5h-Reset"
	claude5hStatus      = "Anthropic-Ratelimit-Unified-5h-Status"

	weeklyWindow = 7 * 24 * time.Hour
)

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginMetadata           `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type pluginMetadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Default     any    `json:"Default,omitempty"`
	Description string `json:"Description"`
}

type registrationCapabilities struct {
	Scheduler      bool `json:"scheduler"`
	ModelRegistrar bool `json:"model_registrar"`
	ModelRouter    bool `json:"model_router"`
	ManagementAPI  bool `json:"management_api"`
}

type pluginConfig struct {
	// MinRemaining is the fraction a credential's windows must still have to be ranked.
	MinRemaining float64 `yaml:"min_remaining" json:"min_remaining"`
	// TieWindowMinutes groups weekly resets this close together and prefers the one with more remaining.
	TieWindowMinutes int              `yaml:"tie_window_minutes" json:"tie_window_minutes"`
	Management       managementConfig `yaml:"management" json:"management"`
	Spread           spreadConfig     `yaml:"spread" json:"spread"`
}

// managementConfig reaches the CPA management API, which proxies provider usage endpoints.
type managementConfig struct {
	URL            string `yaml:"url" json:"url"`
	Key            string `yaml:"key" json:"-"`
	RefreshMinutes int    `yaml:"refresh_minutes" json:"refresh_minutes"`
}

// spreadConfig exposes <Prefix><model> virtual models that rotate across a credential pool.
type spreadConfig struct {
	Prefix string `yaml:"prefix" json:"prefix"`
	// IdleMinutes is how long a session may go without a prompt before it can move to another credential.
	IdleMinutes int              `yaml:"idle_minutes" json:"idle_minutes"`
	Models      []string         `yaml:"models" json:"models"`
	Credentials []poolCredential `yaml:"credentials" json:"credentials"`
}

type poolCredential struct {
	// ID is the host auth ID (the auth file name for file credentials).
	ID       string  `yaml:"id" json:"id"`
	Provider string  `yaml:"provider" json:"provider"`
	Weight   float64 `yaml:"weight" json:"weight"`
}

type schedulerPickRequest struct {
	Model      string               `json:"Model"`
	Options    schedulerOptions     `json:"Options"`
	Candidates []schedulerCandidate `json:"Candidates"`
}

type schedulerOptions struct {
	Metadata map[string]any `json:"Metadata"`
}

type schedulerCandidate struct {
	ID       string         `json:"ID"`
	Provider string         `json:"Provider"`
	Metadata map[string]any `json:"Metadata"`
}

type schedulerPickResponse struct {
	AuthID       string `json:"AuthID,omitempty"`
	Handled      bool   `json:"Handled"`
	Reject       bool   `json:"Reject,omitempty"`
	RejectReason string `json:"RejectReason,omitempty"`
}

type rankedCandidate struct {
	id        string
	reset     time.Time
	remaining float64
}

var activeConfig atomic.Value

func init() {
	activeConfig.Store(defaultPluginConfig())
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		MinRemaining:     0.02,
		TieWindowMinutes: 60,
		Management:       managementConfig{URL: "http://127.0.0.1:8317", RefreshMinutes: 5},
		Spread:           spreadConfig{Prefix: "spread/", IdleMinutes: 5},
	}
}

func currentConfig() pluginConfig {
	return activeConfig.Load().(pluginConfig)
}

func configurePlugin(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if cfg.MinRemaining < 0 || cfg.MinRemaining >= 1 {
		return fmt.Errorf("min_remaining must be in [0, 1), got %v", cfg.MinRemaining)
	}
	if cfg.TieWindowMinutes < 0 {
		return fmt.Errorf("tie_window_minutes must not be negative, got %d", cfg.TieWindowMinutes)
	}
	if errSpread := validateSpread(&cfg); errSpread != nil {
		return errSpread
	}
	activeConfig.Store(cfg)
	return nil
}

func validateSpread(cfg *pluginConfig) error {
	if len(cfg.Spread.Models) == 0 {
		return nil
	}
	if cfg.Spread.Prefix == "" {
		return fmt.Errorf("spread.prefix must not be empty when spread.models is set")
	}
	if cfg.Spread.IdleMinutes <= 0 {
		return fmt.Errorf("spread.idle_minutes must be positive, got %d", cfg.Spread.IdleMinutes)
	}
	if len(cfg.Spread.Credentials) == 0 {
		return fmt.Errorf("spread.credentials must list the pool when spread.models is set")
	}
	if cfg.Management.URL == "" || cfg.Management.Key == "" {
		return fmt.Errorf("management.url and management.key are required for spread quota collection")
	}
	if cfg.Management.RefreshMinutes <= 0 {
		return fmt.Errorf("management.refresh_minutes must be positive, got %d", cfg.Management.RefreshMinutes)
	}
	for index := range cfg.Spread.Credentials {
		credential := &cfg.Spread.Credentials[index]
		if credential.ID == "" || credential.Provider == "" {
			return fmt.Errorf("spread.credentials[%d] needs id and provider", index)
		}
		if credential.Weight < 0 {
			return fmt.Errorf("spread.credentials[%d] weight must not be negative, got %v", index, credential.Weight)
		}
		// Weight is optional; an unset weight counts every credential equally.
		if credential.Weight == 0 {
			credential.Weight = 1
		}
	}
	return nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			ConfigFields: []configField{
				{
					Name:        "min_remaining",
					Type:        "number",
					Default:     0.02,
					Description: "Quota fraction (0-1) every window of a credential must still have to be preferred.",
				},
				{
					Name:        "tie_window_minutes",
					Type:        "integer",
					Default:     60,
					Description: "Weekly resets within this many minutes of the earliest one count as tied; the credential with more remaining wins.",
				},
				{
					Name:        "management",
					Type:        "object",
					Description: "CPA management API used to read provider quota ({url, key, refresh_minutes}).",
				},
				{
					Name:        "spread",
					Type:        "object",
					Description: "Virtual <prefix><model> models that rotate across a credential pool by remaining quota per hour to reset ({prefix, idle_minutes, models, credentials: [{id, provider, weight}]}).",
				},
			},
		},
		Capabilities: registrationCapabilities{Scheduler: true, ModelRegistrar: true, ModelRouter: true, ManagementAPI: true},
	}
}

// pick routes to the Claude credential whose weekly window resets soonest, so quota
// that is about to expire is spent before quota that would carry over. Credentials
// without an observed weekly window are left to the host's configured selector,
// which only runs when no candidate can be ranked.
func pick(req schedulerPickRequest, cfg pluginConfig, now time.Time) (schedulerPickResponse, error) {
	stateMu.Lock()
	for _, candidate := range req.Candidates {
		rawSignals, present := candidate.Metadata[metadataQuotaSignals]
		if !present {
			stateMu.Unlock()
			return schedulerPickResponse{}, fmt.Errorf("candidate %s has no %s metadata: this CLIProxyAPI build does not expose quota signals to scheduler plugins", candidate.ID, metadataQuotaSignals)
		}
		if signals, okSignals := rawSignals.(map[string]any); okSignals {
			lastSignals[candidate.ID] = signals
		}
	}
	stateMu.Unlock()

	requested, okRequested := req.Options.Metadata[metadataRequestedModel].(string)
	if !okRequested {
		return schedulerPickResponse{}, fmt.Errorf("scheduler request has no %s metadata", metadataRequestedModel)
	}
	if cfg.Spread.Prefix != "" && strings.HasPrefix(requested, cfg.Spread.Prefix) {
		return pickSpread(req, cfg, now)
	}

	ranked := make([]rankedCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		rawSignals := candidate.Metadata[metadataQuotaSignals]
		if candidate.Provider != "claude" || rawSignals == nil {
			continue
		}
		signals, okSignals := rawSignals.(map[string]any)
		if !okSignals {
			return schedulerPickResponse{}, fmt.Errorf("candidate %s: %s is %T, want object", candidate.ID, metadataQuotaSignals, rawSignals)
		}
		entry, okRank, errRank := rankClaude(candidate.ID, signals, cfg, now)
		if errRank != nil {
			return schedulerPickResponse{}, errRank
		}
		if okRank {
			ranked = append(ranked, entry)
		}
	}
	if len(ranked) == 0 {
		return schedulerPickResponse{Handled: false}, nil
	}

	sort.Slice(ranked, func(i, j int) bool {
		if !ranked[i].reset.Equal(ranked[j].reset) {
			return ranked[i].reset.Before(ranked[j].reset)
		}
		return ranked[i].id < ranked[j].id
	})
	tieLimit := ranked[0].reset.Add(time.Duration(cfg.TieWindowMinutes) * time.Minute)
	best := ranked[0]
	for _, entry := range ranked[1:] {
		if entry.reset.After(tieLimit) {
			break
		}
		if entry.remaining > best.remaining {
			best = entry
		}
	}
	stateMu.Lock()
	recordDecision(decision{Time: now, Kind: "reset-first", Model: requested, AuthID: best.id, Reason: fmt.Sprintf("earliest weekly reset %s, %.0f%% left", best.reset.Format(time.RFC3339), best.remaining*100)})
	stateMu.Unlock()
	return schedulerPickResponse{AuthID: best.id, Handled: true}, nil
}

// rankClaude derives the weekly reset and remaining fraction from the last observed
// Anthropic unified rate-limit headers. A snapshot without weekly headers (for example
// an API-key credential or a response that only carried Retry-After) is not ranked.
func rankClaude(id string, signals map[string]any, cfg pluginConfig, now time.Time) (rankedCandidate, bool, error) {
	rawReset, hasReset := signals[claude7dReset]
	rawUtilization, hasUtilization := signals[claude7dUtilization]
	if !hasReset || !hasUtilization {
		return rankedCandidate{}, false, nil
	}
	reset, errReset := parseUnixSignal(id, claude7dReset, rawReset)
	if errReset != nil {
		return rankedCandidate{}, false, errReset
	}
	utilization, errUtilization := parseFloatSignal(id, claude7dUtilization, rawUtilization)
	if errUtilization != nil {
		return rankedCandidate{}, false, errUtilization
	}

	if !reset.After(now) {
		// The observed window has rolled over since the snapshot; its usage no longer applies.
		periods := math.Floor(float64(now.Sub(reset))/float64(weeklyWindow)) + 1
		reset = reset.Add(time.Duration(periods) * weeklyWindow)
		utilization = 0
	} else if signalString(signals, claude7dStatus) == "rejected" {
		return rankedCandidate{}, false, nil
	}

	if signalString(signals, claude5hStatus) == "rejected" {
		rawReset5h, hasReset5h := signals[claude5hReset]
		if !hasReset5h {
			return rankedCandidate{}, false, nil
		}
		reset5h, errReset5h := parseUnixSignal(id, claude5hReset, rawReset5h)
		if errReset5h != nil {
			return rankedCandidate{}, false, errReset5h
		}
		if reset5h.After(now) {
			return rankedCandidate{}, false, nil
		}
	}

	remaining := 1 - utilization
	if remaining < cfg.MinRemaining {
		return rankedCandidate{}, false, nil
	}
	return rankedCandidate{id: id, reset: reset, remaining: remaining}, true, nil
}

func signalString(signals map[string]any, key string) string {
	value, _ := signals[key].(string)
	return strings.ToLower(strings.TrimSpace(value))
}

func parseUnixSignal(id, key string, raw any) (time.Time, error) {
	seconds, errParse := parseFloatSignal(id, key, raw)
	if errParse != nil {
		return time.Time{}, errParse
	}
	return time.Unix(int64(seconds), 0), nil
}

func parseFloatSignal(id, key string, raw any) (float64, error) {
	text, okText := raw.(string)
	if !okText {
		return 0, fmt.Errorf("candidate %s: signal %s is %T, want string", id, key, raw)
	}
	value, errParse := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if errParse != nil {
		return 0, fmt.Errorf("candidate %s: signal %s: %w", id, key, errParse)
	}
	return value, nil
}
