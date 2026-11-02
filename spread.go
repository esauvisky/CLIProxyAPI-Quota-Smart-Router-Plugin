package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	methodHostAuthList   = "host.auth.list"
	methodHostAuthModels = "host.auth.models"

	// metadataRouteAuthID and metadataRequestedModel are SchedulerOptions.Metadata keys set by the host
	// (pluginapi.ModelRouteAuthIDMetadataKey and the executor's requested_model).
	metadataRouteAuthID    = "model_route_auth_id"
	metadataRequestedModel = "requested_model"

	hostCacheTTL     = 15 * time.Second
	maxDecisions     = 200
	minHoursToReset  = 0.25
	sessionRetention = 24 * time.Hour
)

type modelRegistrationRequest struct {
	MultiProviderModels bool `json:"multi_provider_models"`
}

type modelInfo struct {
	ID          string `json:"ID"`
	Object      string `json:"Object"`
	OwnedBy     string `json:"OwnedBy"`
	DisplayName string `json:"DisplayName"`
}

type providerModels struct {
	Provider string      `json:"provider"`
	Models   []modelInfo `json:"models"`
}

type modelRegistrationResponse struct {
	Registrations []providerModels `json:"registrations"`
}

type modelRouteRequest struct {
	RequestedModel     string              `json:"RequestedModel"`
	Headers            map[string][]string `json:"Headers"`
	Body               []byte              `json:"Body"`
	AvailableProviders []string            `json:"AvailableProviders"`
	HostCallbackID     string              `json:"host_callback_id"`
}

type modelRouteResponse struct {
	Handled      bool   `json:"Handled"`
	TargetKind   string `json:"TargetKind,omitempty"`
	Target       string `json:"Target,omitempty"`
	TargetModel  string `json:"TargetModel,omitempty"`
	TargetAuthID string `json:"TargetAuthID,omitempty"`
	Reason       string `json:"Reason,omitempty"`
}

type hostAuthListResponse struct {
	Files []hostAuth `json:"files"`
}

type hostAuthModelsResponse struct {
	Credentials []struct {
		ID     string   `json:"id"`
		Models []string `json:"models"`
	} `json:"credentials"`
}

type hostSnapshot struct {
	auths     []hostAuth
	models    map[string][]string
	fetchedAt time.Time
}

var (
	hostMu    sync.Mutex
	hostCache hostSnapshot

	stateMu   sync.Mutex
	sessions  = make(map[string]*sessionBinding)
	decisions []decision
	// hostCall is the host callback entry point; unit tests replace it because they run without a host.
	hostCall = callHost

	// lastSignals keeps the passive header snapshot of every candidate the scheduler saw, for the UI.
	lastSignals = make(map[string]map[string]any)
)

// registerSpreadModels publishes <prefix><model> under every provider that has a pool credential.
func registerSpreadModels(raw []byte) ([]byte, error) {
	var req modelRegistrationRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := currentConfig().Spread
	if len(cfg.Models) == 0 {
		return okEnvelope(modelRegistrationResponse{})
	}
	if !req.MultiProviderModels {
		return nil, fmt.Errorf("host does not support multi-provider model registration")
	}
	var registrations []providerModels
	seen := make(map[string]bool)
	for _, credential := range cfg.Credentials {
		if seen[credential.Provider] {
			continue
		}
		seen[credential.Provider] = true
		models := make([]modelInfo, 0, len(cfg.Models))
		for _, model := range cfg.Models {
			models = append(models, modelInfo{
				ID:          cfg.Prefix + model,
				Object:      "model",
				OwnedBy:     pluginName,
				DisplayName: model + " (spread)",
			})
		}
		registrations = append(registrations, providerModels{Provider: credential.Provider, Models: models})
	}
	return okEnvelope(modelRegistrationResponse{Registrations: registrations})
}

// hostState returns the credential list and per-credential models, cached for hostCacheTTL.
func hostState(now time.Time) (hostSnapshot, error) {
	hostMu.Lock()
	defer hostMu.Unlock()
	if !hostCache.fetchedAt.IsZero() && now.Sub(hostCache.fetchedAt) < hostCacheTTL {
		return hostCache, nil
	}
	rawList, errList := hostCall(methodHostAuthList, struct{}{})
	if errList != nil {
		return hostSnapshot{}, errList
	}
	var list hostAuthListResponse
	if errUnmarshal := json.Unmarshal(rawList, &list); errUnmarshal != nil {
		return hostSnapshot{}, fmt.Errorf("decode host.auth.list: %w", errUnmarshal)
	}
	rawModels, errModels := hostCall(methodHostAuthModels, struct{}{})
	if errModels != nil {
		return hostSnapshot{}, errModels
	}
	var modelsResp hostAuthModelsResponse
	if errUnmarshal := json.Unmarshal(rawModels, &modelsResp); errUnmarshal != nil {
		return hostSnapshot{}, fmt.Errorf("decode host.auth.models: %w", errUnmarshal)
	}
	models := make(map[string][]string, len(modelsResp.Credentials))
	for _, credential := range modelsResp.Credentials {
		models[credential.ID] = credential.Models
	}
	hostCache = hostSnapshot{auths: list.Files, models: models, fetchedAt: now}
	return hostCache, nil
}

// poolAuths resolves the configured pool against the host credential list.
func poolAuths(cfg spreadConfig, snapshot hostSnapshot) ([]hostAuth, error) {
	byID := make(map[string]hostAuth, len(snapshot.auths))
	for _, auth := range snapshot.auths {
		byID[auth.ID] = auth
	}
	out := make([]hostAuth, 0, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		auth, ok := byID[credential.ID]
		if !ok {
			return nil, fmt.Errorf("spread pool credential %s is not registered in the host", credential.ID)
		}
		if auth.Provider != credential.Provider {
			return nil, fmt.Errorf("spread pool credential %s has provider %s in the host, config says %s", credential.ID, auth.Provider, credential.Provider)
		}
		out = append(out, auth)
	}
	return out, nil
}

func poolWeight(cfg spreadConfig, authID string) (float64, bool) {
	for _, credential := range cfg.Credentials {
		if credential.ID == authID {
			return credential.Weight, true
		}
	}
	return 0, false
}

// routeSpread handles <prefix><model> requests: it keeps an active session on its credential and
// otherwise picks the pool credential with the highest weighted remaining quota per hour to reset.
func routeSpread(raw []byte) ([]byte, error) {
	var req modelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := currentConfig()
	if cfg.Spread.Prefix == "" || !strings.HasPrefix(req.RequestedModel, cfg.Spread.Prefix) {
		return okEnvelope(modelRouteResponse{Handled: false})
	}
	model := strings.TrimPrefix(req.RequestedModel, cfg.Spread.Prefix)
	now := time.Now()
	snapshot, errHost := hostState(now)
	if errHost != nil {
		return nil, errHost
	}
	pool, errPool := poolAuths(cfg.Spread, snapshot)
	if errPool != nil {
		return nil, errPool
	}
	// A credential without any snapshot (plugin just loaded) is fetched before ranking; later
	// refreshes run in the background.
	var unfetched []hostAuth
	for _, auth := range pool {
		if _, ok := quotaFor(auth.ID); !ok {
			unfetched = append(unfetched, auth)
		}
	}
	refreshQuotaNow(cfg.Management, unfetched)
	ensureQuotaFresh(cfg.Management, pool)

	session, errSession := sessionKey(req.Headers, req.Body)
	if errSession != nil {
		return nil, errSession
	}
	ranking := rankPool(cfg, pool, snapshot.models, model, now)

	stateMu.Lock()
	defer stateMu.Unlock()
	if session != "" {
		if binding, ok := sessions[session]; ok && now.Before(binding.IdleExpiresAt) && binding.Model == model {
			if entry, found := findRank(ranking, binding.AuthID); found && entry.Excluded == "" {
				touchSession(binding, cfg.Spread, now)
				recordDecision(decision{Time: now, Kind: "route", Model: req.RequestedModel, Session: session, AuthID: binding.AuthID, Reason: "session active, kept credential", Ranking: ranking})
				return okEnvelope(routeTo(binding.Provider, model, binding.AuthID))
			}
		}
	}
	best, ok := bestRank(ranking)
	if !ok {
		recordDecision(decision{Time: now, Kind: "route", Model: req.RequestedModel, Session: session, Reason: "no eligible pool credential", Ranking: ranking})
		return nil, fmt.Errorf("spread: no eligible pool credential serves %s", model)
	}
	reason := "highest rate"
	if session != "" {
		sessions[session] = &sessionBinding{Session: session, AuthID: best.AuthID, Provider: best.Provider, Model: model, BoundAt: now}
		touchSession(sessions[session], cfg.Spread, now)
		reason = "new or idle session bound to highest rate"
	}
	recordDecision(decision{Time: now, Kind: "route", Model: req.RequestedModel, Session: session, AuthID: best.AuthID, Reason: reason, Ranking: ranking})
	pruneSessions(now)
	return okEnvelope(routeTo(best.Provider, model, best.AuthID))
}

func routeTo(provider, model, authID string) modelRouteResponse {
	return modelRouteResponse{Handled: true, TargetKind: "provider", Target: provider, TargetModel: model, TargetAuthID: authID, Reason: "spread"}
}

func touchSession(binding *sessionBinding, cfg spreadConfig, now time.Time) {
	binding.LastSeen = now
	binding.IdleExpiresAt = now.Add(time.Duration(cfg.IdleMinutes) * time.Minute)
}

func pruneSessions(now time.Time) {
	for key, binding := range sessions {
		if now.Sub(binding.LastSeen) > sessionRetention {
			delete(sessions, key)
		}
	}
}

// rankPool evaluates every pool credential for a model, ranked best first; excluded ones go last.
func rankPool(cfg pluginConfig, pool []hostAuth, models map[string][]string, model string, now time.Time) []rankEntry {
	ranking := make([]rankEntry, 0, len(pool))
	for _, auth := range pool {
		weight, _ := poolWeight(cfg.Spread, auth.ID)
		entry := rankEntry{AuthID: auth.ID, Provider: auth.Provider, Weight: weight}
		switch {
		case auth.Disabled:
			entry.Excluded = "disabled"
		case auth.Unavailable && auth.NextRetryAfter.After(now):
			entry.Excluded = "cooling down until " + auth.NextRetryAfter.Format(time.RFC3339)
		case !containsString(models[auth.ID], model):
			entry.Excluded = "does not serve " + model
		default:
			rateCredential(&entry, model, cfg.MinRemaining, now)
		}
		ranking = append(ranking, entry)
	}
	sort.SliceStable(ranking, func(i, j int) bool {
		if (ranking[i].Excluded == "") != (ranking[j].Excluded == "") {
			return ranking[i].Excluded == ""
		}
		return ranking[i].Rate > ranking[j].Rate
	})
	return ranking
}

// rateCredential scores a credential from its quota windows that apply to the model. The longest
// window governs the rate (quota that resets later can wait); every applicable window must keep
// at least minRemaining.
func rateCredential(entry *rankEntry, model string, minRemaining float64, now time.Time) {
	quota, ok := quotaFor(entry.AuthID)
	if !ok {
		entry.Excluded = "quota not fetched yet"
		return
	}
	if quota.Error != "" {
		entry.Excluded = "quota error: " + quota.Error
		return
	}
	var governing *quotaWindow
	for index := range quota.Windows {
		window := &quota.Windows[index]
		if len(window.Models) > 0 && !containsString(window.Models, model) {
			continue
		}
		remaining, resetAt := rolledWindow(*window, now)
		if remaining < minRemaining {
			entry.Excluded = fmt.Sprintf("%s has %.0f%% left", window.Name, remaining*100)
			return
		}
		if governing == nil || resetAt.After(governingReset(*governing, now)) {
			governing = window
		}
	}
	if governing == nil {
		entry.Excluded = "no quota window applies to " + model
		return
	}
	remaining, resetAt := rolledWindow(*governing, now)
	if resetAt.IsZero() {
		entry.Excluded = governing.Name + " has no reset time"
		return
	}
	hours := math.Max(resetAt.Sub(now).Hours(), minHoursToReset)
	entry.Window = governing.Name
	entry.Remaining = remaining
	entry.HoursToReset = hours
	entry.Rate = entry.Weight * remaining / hours
}

// rolledWindow applies resets that happened since the snapshot: a passed reset with a known
// period means the window is full again until the next period boundary.
func rolledWindow(window quotaWindow, now time.Time) (float64, time.Time) {
	if window.ResetAt.IsZero() || window.ResetAt.After(now) || window.Period <= 0 {
		return window.Remaining, window.ResetAt
	}
	periods := math.Floor(float64(now.Sub(window.ResetAt))/float64(window.Period)) + 1
	return 1, window.ResetAt.Add(time.Duration(periods) * window.Period)
}

func governingReset(window quotaWindow, now time.Time) time.Time {
	_, resetAt := rolledWindow(window, now)
	return resetAt
}

func bestRank(ranking []rankEntry) (rankEntry, bool) {
	if len(ranking) == 0 || ranking[0].Excluded != "" {
		return rankEntry{}, false
	}
	return ranking[0], true
}

func findRank(ranking []rankEntry, authID string) (rankEntry, bool) {
	for _, entry := range ranking {
		if entry.AuthID == authID {
			return entry, true
		}
	}
	return rankEntry{}, false
}

// pickSpread runs in the scheduler for spread requests: it honors the router's credential and,
// when that one is no longer a candidate (it failed and the host is retrying), picks the best
// remaining pool credential of the routed provider.
func pickSpread(req schedulerPickRequest, cfg pluginConfig, now time.Time) (schedulerPickResponse, error) {
	requested, okRequested := req.Options.Metadata[metadataRequestedModel].(string)
	if !okRequested {
		return schedulerPickResponse{}, fmt.Errorf("scheduler request has no %s metadata", metadataRequestedModel)
	}
	// The router sets the preferred credential on every spread route; a missing key means the
	// host build does not forward ModelRouteResponse.TargetAuthID.
	preferred, okPreferred := req.Options.Metadata[metadataRouteAuthID].(string)
	if !okPreferred {
		return schedulerPickResponse{}, fmt.Errorf("spread request %s has no %s metadata: this CLIProxyAPI build does not forward router credential choices", requested, metadataRouteAuthID)
	}
	candidates := make(map[string]bool, len(req.Candidates))
	for _, candidate := range req.Candidates {
		candidates[candidate.ID] = true
	}
	if candidates[preferred] {
		stateMu.Lock()
		recordDecision(decision{Time: now, Kind: "schedule", Model: requested, AuthID: preferred, Reason: "router choice"})
		stateMu.Unlock()
		return schedulerPickResponse{AuthID: preferred, Handled: true}, nil
	}

	snapshot, errHost := hostState(now)
	if errHost != nil {
		return schedulerPickResponse{}, errHost
	}
	pool, errPool := poolAuths(cfg.Spread, snapshot)
	if errPool != nil {
		return schedulerPickResponse{}, errPool
	}
	remaining := make([]hostAuth, 0, len(pool))
	for _, auth := range pool {
		if candidates[auth.ID] {
			remaining = append(remaining, auth)
		}
	}
	ranking := rankPool(cfg, remaining, snapshot.models, req.Model, now)
	best, ok := bestRank(ranking)
	stateMu.Lock()
	defer stateMu.Unlock()
	if !ok {
		recordDecision(decision{Time: now, Kind: "schedule", Model: requested, Reason: "router choice unavailable, no other eligible pool credential", Ranking: ranking})
		return schedulerPickResponse{Handled: true, Reject: true, RejectReason: "spread pool has no eligible credential for " + req.Model}, nil
	}
	recordDecision(decision{Time: now, Kind: "schedule", Model: requested, AuthID: best.AuthID, Reason: "router choice " + preferred + " unavailable, next highest rate", Ranking: ranking})
	return schedulerPickResponse{AuthID: best.AuthID, Handled: true}, nil
}

// sessionKey identifies a client session from the headers and body fields the host's session
// affinity also uses. An empty key means the request carries no session identity.
func sessionKey(headers map[string][]string, body []byte) (string, error) {
	for _, name := range []string{"X-Claude-Code-Session-Id", "Session-Id", "X-Session-Id", "X-Session-Affinity"} {
		if value := strings.TrimSpace(http.Header(headers).Get(name)); value != "" {
			return name + ":" + value, nil
		}
	}
	if len(body) == 0 {
		return "", nil
	}
	var fields struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		PromptCacheKey string `json:"prompt_cache_key"`
		SessionID      string `json:"session_id"`
	}
	if errUnmarshal := json.Unmarshal(body, &fields); errUnmarshal != nil {
		return "", fmt.Errorf("spread: decode request body for session identity: %w", errUnmarshal)
	}
	switch {
	case fields.Metadata.UserID != "":
		return "user_id:" + fields.Metadata.UserID, nil
	case fields.PromptCacheKey != "":
		return "pck:" + fields.PromptCacheKey, nil
	case fields.SessionID != "":
		return "session_id:" + fields.SessionID, nil
	}
	return "", nil
}

// recordDecision appends to the ring buffer; callers hold stateMu.
func recordDecision(entry decision) {
	decisions = append(decisions, entry)
	if len(decisions) > maxDecisions {
		decisions = decisions[len(decisions)-maxDecisions:]
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func buildUIState() uiState {
	now := time.Now()
	cfg := currentConfig()
	state := uiState{Version: pluginVersion, Now: now, Config: cfg}
	snapshot, errHost := hostState(now)
	if errHost != nil {
		state.Error = errHost.Error()
	}
	if cfg.Management.URL != "" {
		ensureQuotaFresh(cfg.Management, snapshot.auths)
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	for _, auth := range snapshot.auths {
		weight, inPool := poolWeight(cfg.Spread, auth.ID)
		quota, hasQuota := quotaFor(auth.ID)
		state.Credentials = append(state.Credentials, uiCredential{Auth: auth, InPool: inPool, Weight: weight, Quota: quota, HasQuota: hasQuota, Signals: lastSignals[auth.ID]})
	}
	for _, binding := range sessions {
		state.Sessions = append(state.Sessions, *binding)
	}
	sort.Slice(state.Sessions, func(i, j int) bool { return state.Sessions[i].LastSeen.After(state.Sessions[j].LastSeen) })
	for index := len(decisions) - 1; index >= 0; index-- {
		state.Decisions = append(state.Decisions, decisions[index])
	}
	return state
}
