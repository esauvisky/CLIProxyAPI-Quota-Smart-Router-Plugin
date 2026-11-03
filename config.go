package main

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// groupProvider is the executor identifier model groups are registered and executed under.
const groupProvider = "model-groups"

// executorFormats lists every client protocol; group requests pass through in the client's own format.
var executorFormats = []string{"openai", "openai-response", "claude", "gemini", "codex", "antigravity", "interactions"}

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
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Default     any    `json:"Default,omitempty"`
	Description string `json:"Description"`
}

type registrationCapabilities struct {
	Scheduler             bool     `json:"scheduler"`
	ModelRegistrar        bool     `json:"model_registrar"`
	ModelRouter           bool     `json:"model_router"`
	ManagementAPI         bool     `json:"management_api"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	RequestInterceptor    bool     `json:"request_interceptor"`
}

type pluginConfig struct {
	// MinRemaining is the fraction every applicable quota window must keep for a credential to be used.
	MinRemaining float64 `yaml:"min_remaining" json:"min_remaining"`
	// IdleMinutes is how long a session may go without a prompt before it can move to another credential.
	IdleMinutes   int           `yaml:"idle_minutes" json:"idle_minutes"`
	ManagementKey string        `yaml:"management_key" json:"-"`
	Groups        []modelGroup  `yaml:"groups" json:"groups"`
	Features      featureConfig `yaml:"features" json:"features"`
	// LegacyManagement reads existing management.key configs on upgrade.
	LegacyManagement struct {
		Key string `yaml:"key" json:"key"`
	} `yaml:"management,omitempty" json:"-"`
}
// featureConfig controls safeguard and client-wire adaptations.
type featureConfig struct {
	FallbackOnRefusal             bool   `yaml:"fallback_on_refusal" json:"fallback_on_refusal"`
	ClientFallbackNotices         bool   `yaml:"client_fallback_notices" json:"client_fallback_notices"`
	NoticePrefix                  string `yaml:"notice_prefix" json:"notice_prefix"`
	RewriteCyberSystemPrompt      bool   `yaml:"rewrite_cyber_system_prompt" json:"rewrite_cyber_system_prompt"`
	StripRefusalTaintHeaders      bool   `yaml:"strip_refusal_taint_headers" json:"strip_refusal_taint_headers"`
	DisableUpstreamServerFallback bool   `yaml:"disable_upstream_server_fallback" json:"disable_upstream_server_fallback"`
	BypassAutoModeClassifier      bool   `yaml:"bypass_auto_mode_classifier" json:"bypass_auto_mode_classifier"`
	NeutralizeClassifierStopNudge bool   `yaml:"neutralize_classifier_stop_nudges" json:"neutralize_classifier_stop_nudges"`
	StripRestrictiveReminders     bool   `yaml:"strip_restrictive_system_reminders" json:"strip_restrictive_system_reminders"`
}

func (f featureConfig) effectiveNoticePrefix() string {
	if f.NoticePrefix != "" {
		return f.NoticePrefix
	}
	return defaultNoticePrefix
}

// modelGroup is a virtual model that tries its target models in order until one succeeds.
type modelGroup struct {
	// ID is the model codename clients request.
	ID string `yaml:"id" json:"id"`
	// Name is the display name shown in model lists.
	Name   string   `yaml:"name" json:"name"`
	Models []string `yaml:"models" json:"models"`
}

var activeConfig atomic.Value

func init() {
	activeConfig.Store(defaultPluginConfig())
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		MinRemaining: 0.02,
		IdleMinutes:  5,
		Features: featureConfig{
			FallbackOnRefusal:             true,
			ClientFallbackNotices:         true,
			NoticePrefix:                  defaultNoticePrefix,
			RewriteCyberSystemPrompt:      true,
			StripRefusalTaintHeaders:      true,
			DisableUpstreamServerFallback: true,
			BypassAutoModeClassifier:      true,
			NeutralizeClassifierStopNudge: true,
			StripRestrictiveReminders:     true,
		},
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
	if cfg.ManagementKey == "" && cfg.LegacyManagement.Key != "" {
		cfg.ManagementKey = cfg.LegacyManagement.Key
	}
	if errValidate := validateConfig(cfg); errValidate != nil {
		return errValidate
	}
	activeConfig.Store(cfg)
	return nil
}

func validateConfig(cfg pluginConfig) error {
	if cfg.MinRemaining < 0 || cfg.MinRemaining >= 1 {
		return fmt.Errorf("min_remaining must be in [0, 1), got %v", cfg.MinRemaining)
	}
	if cfg.IdleMinutes <= 0 {
		return fmt.Errorf("idle_minutes must be positive, got %d", cfg.IdleMinutes)
	}
	ids := make(map[string]bool, len(cfg.Groups))
	for index, group := range cfg.Groups {
		if group.ID == "" {
			return fmt.Errorf("groups[%d] needs an id", index)
		}
		if ids[group.ID] {
			return fmt.Errorf("group id %s is used twice", group.ID)
		}
		ids[group.ID] = true
		if group.Name == "" {
			return fmt.Errorf("group %s needs a name", group.ID)
		}
		if len(group.Models) == 0 {
			return fmt.Errorf("group %s needs at least one model", group.ID)
		}
	}
	for _, group := range cfg.Groups {
		for _, model := range group.Models {
			if ids[model] {
				return fmt.Errorf("group %s lists group %s as a target; groups cannot nest", group.ID, model)
			}
		}
	}
	return nil
}

func (cfg pluginConfig) group(id string) (modelGroup, bool) {
	for _, group := range cfg.Groups {
		if group.ID == id {
			return group, true
		}
	}
	return modelGroup{}, false
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			Logo:             pluginLogo,
			ConfigFields: []configField{
				{Name: "management_key", Type: "string", Description: "CPA management key used to read provider quota."},
				{Name: "min_remaining", Type: "number", Default: 0.02, Description: "Quota fraction (0-1) every window of a credential must keep for it to be used."},
				{Name: "idle_minutes", Type: "integer", Default: 5, Description: "Minutes without a prompt after which a session may move to another credential."},
				{Name: "groups", Type: "array", Description: "Model groups ({id, name, models}): a virtual model that tries its models in order until one succeeds."},
				{Name: "features", Type: "object", Description: "Safeguard and client-wire adaptation toggles and custom notice_prefix."},
			},
		},
		Capabilities: registrationCapabilities{
			Scheduler:             true,
			ModelRegistrar:        true,
			ModelRouter:           true,
			ManagementAPI:         true,
			Executor:              true,
			ExecutorModelScope:    "static",
			ExecutorInputFormats:  executorFormats,
			ExecutorOutputFormats: executorFormats,
			RequestInterceptor:    true,
		},
	}
}
