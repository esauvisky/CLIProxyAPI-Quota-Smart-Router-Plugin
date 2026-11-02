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
}

type pluginConfig struct {
	// MinRemaining is the fraction every applicable quota window must keep for a credential to be used.
	MinRemaining float64 `yaml:"min_remaining" json:"min_remaining"`
	// IdleMinutes is how long a session may go without a prompt before it can move to another credential.
	IdleMinutes int              `yaml:"idle_minutes" json:"idle_minutes"`
	Management  managementConfig `yaml:"management" json:"management"`
	// Weights scales a credential's rate by auth ID (plan size); unlisted credentials weigh 1.
	Weights map[string]float64 `yaml:"weights" json:"weights"`
	Groups  []modelGroup       `yaml:"groups" json:"groups"`
}

// managementConfig reaches the CPA management API, which proxies provider usage endpoints.
type managementConfig struct {
	URL            string `yaml:"url" json:"url"`
	Key            string `yaml:"key" json:"-"`
	RefreshMinutes int    `yaml:"refresh_minutes" json:"refresh_minutes"`
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
		Management:   managementConfig{URL: "http://127.0.0.1:8317", RefreshMinutes: 5},
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
	if cfg.Management.RefreshMinutes <= 0 {
		return fmt.Errorf("management.refresh_minutes must be positive, got %d", cfg.Management.RefreshMinutes)
	}
	for authID, weight := range cfg.Weights {
		if weight <= 0 {
			return fmt.Errorf("weights[%s] must be positive, got %v", authID, weight)
		}
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

func (cfg pluginConfig) weight(authID string) float64 {
	if weight, ok := cfg.Weights[authID]; ok {
		return weight
	}
	return 1
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
				{Name: "min_remaining", Type: "number", Default: 0.02, Description: "Quota fraction (0-1) every window of a credential must keep for it to be used."},
				{Name: "idle_minutes", Type: "integer", Default: 5, Description: "Minutes without a prompt after which a session may move to another credential."},
				{Name: "management", Type: "object", Description: "CPA management API used to read provider quota ({url, key, refresh_minutes})."},
				{Name: "weights", Type: "object", Description: "Per-credential rate multiplier by auth ID, e.g. 20 for Max 20x (default 1)."},
				{Name: "groups", Type: "array", Description: "Model groups ({id, name, models}): a virtual model that tries its models in order until one succeeds."},
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
		},
	}
}
