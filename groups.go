package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	methodHostModelExecute     = "host.model.execute"
	methodHostModelStream      = "host.model.execute_stream"
	methodHostModelStreamRead  = "host.model.stream_read"
	methodHostModelStreamClose = "host.model.stream_close"
	methodHostStreamEmit       = "host.stream.emit"
	methodHostStreamClose      = "host.stream.close"
)

type modelInfo struct {
	ID          string `json:"ID"`
	Object      string `json:"Object"`
	OwnedBy     string `json:"OwnedBy"`
	DisplayName string `json:"DisplayName"`
}

type modelRegistrationResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

type modelRouteRequest struct {
	RequestedModel string `json:"RequestedModel"`
}

type modelRouteResponse struct {
	Handled    bool   `json:"Handled"`
	TargetKind string `json:"TargetKind,omitempty"`
	Reason     string `json:"Reason,omitempty"`
}

type executorRequest struct {
	Model          string         `json:"Model"`
	Format         string         `json:"Format"`
	Stream         bool           `json:"Stream"`
	Alt            string         `json:"Alt"`
	Headers        http.Header    `json:"Headers"`
	Query          url.Values     `json:"Query"`
	SourceFormat   string         `json:"SourceFormat"`
	Payload        []byte         `json:"Payload"`
	Metadata       map[string]any `json:"Metadata"`
	StreamID       string         `json:"stream_id"`
	HostCallbackID string         `json:"host_callback_id"`
}

type executorResponse struct {
	Payload []byte      `json:"Payload"`
	Headers http.Header `json:"Headers,omitempty"`
}

type executorStreamOpenResponse struct {
	Headers http.Header `json:"headers"`
}

type hostModelRequest struct {
	EntryProtocol  string      `json:"entry_protocol"`
	ExitProtocol   string      `json:"exit_protocol"`
	Model          string      `json:"model"`
	Stream         bool        `json:"stream"`
	Body           []byte      `json:"body"`
	Headers        http.Header `json:"headers"`
	Query          url.Values  `json:"query"`
	Alt            string      `json:"alt"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type hostModelResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
}

type hostModelStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type hostStreamRef struct {
	StreamID       string `json:"stream_id"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostStreamReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

type hostStreamEmit struct {
	StreamID       string `json:"stream_id"`
	Payload        []byte `json:"payload,omitempty"`
	Error          string `json:"error,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// groupAttempt is one target model tried (or skipped) for a group request.
type groupAttempt struct {
	Model  string `json:"model"`
	Result string `json:"result"`
}

func groupModelRegistration(cfg pluginConfig) modelRegistrationResponse {
	models := make([]modelInfo, 0, len(cfg.Groups))
	for _, group := range cfg.Groups {
		models = append(models, modelInfo{ID: group.ID, Object: "model", OwnedBy: groupProvider, DisplayName: group.Name})
	}
	return modelRegistrationResponse{Provider: groupProvider, Models: models}
}

// routeGroup sends requests for group codenames to this plugin's executor.
func routeGroup(raw []byte, cfg pluginConfig) ([]byte, error) {
	var req modelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if _, ok := cfg.group(req.RequestedModel); !ok {
		return okEnvelope(modelRouteResponse{Handled: false})
	}
	return okEnvelope(modelRouteResponse{Handled: true, TargetKind: "self", Reason: "model group"})
}

// executeGroup tries the group's models in order and returns the first successful response.
func executeGroup(raw []byte, cfg pluginConfig) ([]byte, error) {
	var req executorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	group, ok := cfg.group(req.Model)
	if !ok {
		return nil, fmt.Errorf("model group %s is not configured", req.Model)
	}
	now := time.Now()
	session := sessionKey(req.Metadata)
	resetRefusalsIfAllRefused(cfg, session, group, now)
	var attempts []groupAttempt
	var lastErr error
	for _, model := range group.Models {
		if isSessionRefused(cfg, session, group.ID, model, now) {
			attempts = append(attempts, groupAttempt{Model: model, Result: "skipped: refused earlier in session"})
			continue
		}
		reason, errSkip := skipReason(cfg, model, now)
		if errSkip != nil {
			return nil, errSkip
		}
		if reason != "" {
			attempts = append(attempts, groupAttempt{Model: model, Result: "skipped: " + reason})
			continue
		}
		body, errBody := withModel(req.Payload, model)
		if errBody != nil {
			return nil, errBody
		}
		rawResp, errCall := hostCall(methodHostModelExecute, hostModelRequest{
			EntryProtocol: req.SourceFormat, ExitProtocol: req.Format, Model: model, Body: body,
			Headers: req.Headers, Query: req.Query, Alt: req.Alt, HostCallbackID: req.HostCallbackID,
		})
		if errCall != nil {
			attempts = append(attempts, groupAttempt{Model: model, Result: "failed: " + errCall.Error()})
			lastErr = errCall
			continue
		}
		var resp hostModelResponse
		if errDecode := json.Unmarshal(rawResp, &resp); errDecode != nil {
			return nil, fmt.Errorf("decode host.model.execute response: %w", errDecode)
		}
		if errRefusal := checkNonStreamResponse(resp.Body); errRefusal != nil {
			recordSessionRefusal(session, group.ID, model, time.Now())
			attempts = append(attempts, groupAttempt{Model: model, Result: "failed: " + errRefusal.Error()})
			lastErr = errRefusal
			continue
		}
		attempts = append(attempts, groupAttempt{Model: model, Result: "served"})
		recordGroupDecision(group, attempts, model)
		prevModel := updateSessionGroupModel(session, group.ID, model)
		notice := buildGroupNotice(group, attempts, model, prevModel)
		bodyOut := injectNonStreamNotice(resp.Body, effectiveFormat(req.Format, req.SourceFormat), notice)
		return okEnvelope(executorResponse{Payload: bodyOut, Headers: resp.Headers})
	}
	recordGroupDecision(group, attempts, "")
	return nil, groupFailure(group, attempts, lastErr)
}

// executeGroupStream opens the first group model that produces visible output without refusing
// and forwards its chunks.
func executeGroupStream(raw []byte, cfg pluginConfig) ([]byte, error) {
	var req executorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if req.StreamID == "" {
		return nil, fmt.Errorf("executor.execute_stream request has no stream_id")
	}
	group, ok := cfg.group(req.Model)
	if !ok {
		return nil, fmt.Errorf("model group %s is not configured", req.Model)
	}
	now := time.Now()
	session := sessionKey(req.Metadata)
	resetRefusalsIfAllRefused(cfg, session, group, now)
	var attempts []groupAttempt
	var lastErr error
	for _, model := range group.Models {
		if isSessionRefused(cfg, session, group.ID, model, now) {
			attempts = append(attempts, groupAttempt{Model: model, Result: "skipped: refused earlier in session"})
			continue
		}
		reason, errSkip := skipReason(cfg, model, now)
		if errSkip != nil {
			return nil, errSkip
		}
		if reason != "" {
			attempts = append(attempts, groupAttempt{Model: model, Result: "skipped: " + reason})
			continue
		}
		body, errBody := withModel(req.Payload, model)
		if errBody != nil {
			return nil, errBody
		}
		rawResp, errCall := hostCall(methodHostModelStream, hostModelRequest{
			EntryProtocol: req.SourceFormat, ExitProtocol: req.Format, Model: model, Stream: true, Body: body,
			Headers: req.Headers, Query: req.Query, Alt: req.Alt, HostCallbackID: req.HostCallbackID,
		})
		if errCall != nil {
			attempts = append(attempts, groupAttempt{Model: model, Result: "failed: " + errCall.Error()})
			lastErr = errCall
			continue
		}
		var resp hostModelStreamResponse
		if errDecode := json.Unmarshal(rawResp, &resp); errDecode != nil {
			return nil, fmt.Errorf("decode host.model.execute_stream response: %w", errDecode)
		}
		prefetched, done, errPrefetch := prefetchStream(resp.StreamID, req.HostCallbackID)
		if errPrefetch != nil {
			recordSessionRefusal(session, group.ID, model, time.Now())
			attempts = append(attempts, groupAttempt{Model: model, Result: "failed: " + errPrefetch.Error()})
			lastErr = errPrefetch
			continue
		}
		attempts = append(attempts, groupAttempt{Model: model, Result: "served"})
		recordGroupDecision(group, attempts, model)
		prevModel := updateSessionGroupModel(session, group.ID, model)
		notice := buildGroupNotice(group, attempts, model, prevModel)
		prefetched, shiftIndices := injectStreamNotice(prefetched, effectiveFormat(req.Format, req.SourceFormat), notice)
		go forwardStream(resp.StreamID, req.StreamID, req.HostCallbackID, prefetched, done, shiftIndices, session, group.ID, model)
		return okEnvelope(executorStreamOpenResponse{Headers: resp.Headers})
	}
	recordGroupDecision(group, attempts, "")
	return nil, groupFailure(group, attempts, lastErr)
}

// forwardStream flushes prefetched chunks and copies the remainder of the target model's stream.
func forwardStream(source, target, callbackID string, prefetched [][]byte, sourceDone, shiftIndices bool, session, groupID, model string) {
	for _, payload := range prefetched {
		if _, errEmit := hostCall(methodHostStreamEmit, hostStreamEmit{StreamID: target, Payload: payload, HostCallbackID: callbackID}); errEmit != nil {
			if !sourceDone {
				_, _ = hostCall(methodHostModelStreamClose, hostStreamRef{StreamID: source, HostCallbackID: callbackID})
			}
			return
		}
	}
	if sourceDone {
		closeStream(target, callbackID, "")
		return
	}
	for {
		rawRead, errRead := hostCall(methodHostModelStreamRead, hostStreamRef{StreamID: source, HostCallbackID: callbackID})
		if errRead != nil {
			closeStream(target, callbackID, errRead.Error())
			return
		}
		var chunk hostStreamReadResponse
		if errDecode := json.Unmarshal(rawRead, &chunk); errDecode != nil {
			closeStream(target, callbackID, "decode host.model.stream_read response: "+errDecode.Error())
			return
		}
		if len(chunk.Payload) > 0 {
			if refusal, _, _, _ := inspectStreamChunk(chunk.Payload); refusal != "" {
				recordSessionRefusal(session, groupID, model, time.Now())
			}
			payload := chunk.Payload
			if shiftIndices {
				payload, _ = shiftClaudeStreamChunk(payload, false, "")
			}
			if _, errEmit := hostCall(methodHostStreamEmit, hostStreamEmit{StreamID: target, Payload: payload, HostCallbackID: callbackID}); errEmit != nil {
				// The client stream is gone; release the target stream. Its close result has no reader.
				_, _ = hostCall(methodHostModelStreamClose, hostStreamRef{StreamID: source, HostCallbackID: callbackID})
				return
			}
		}
		if chunk.Error != "" || chunk.Done {
			closeStream(target, callbackID, chunk.Error)
			return
		}
	}
}

func closeStream(target, callbackID, errText string) {
	// The host owns the client connection; a failed close has no other channel to report through.
	_, _ = hostCall(methodHostStreamClose, hostStreamEmit{StreamID: target, Error: errText, HostCallbackID: callbackID})
}

// skipReason reports why a target model should not be tried: every credential serving it is
// below min_remaining. Credentials without quota data (API keys, unsupported providers) are
// assumed usable, so a model is only skipped on positive evidence.
func skipReason(cfg pluginConfig, model string, now time.Time) (string, error) {
	if cfg.Management.Key == "" {
		return "", nil
	}
	snapshot, errHost := hostState(now)
	if errHost != nil {
		return "", errHost
	}
	var exhausted []string
	for _, auth := range snapshot.auths {
		if auth.Disabled || !containsString(snapshot.models[auth.ID], model) {
			continue
		}
		entry := rankEntry{AuthID: auth.ID, Weight: 1}
		rateCredential(&entry, model, cfg.MinRemaining, now)
		if entry.Excluded == "" || !strings.Contains(entry.Excluded, "% left") {
			return "", nil
		}
		exhausted = append(exhausted, auth.ID+" ("+entry.Excluded+")")
	}
	if len(exhausted) == 0 {
		return "", nil
	}
	return "all credentials low on quota: " + strings.Join(exhausted, ", "), nil
}

// withModel sets the top-level "model" field the client sent to the target model and neutralizes
// Claude Code's refusal-retry poison nudge so a fallback model continues instead of refusing.
// Protocols that carry the model only in the path (Gemini) have no "model" field and are passed
// through unchanged.
func withModel(payload []byte, model string) ([]byte, error) {
	payload = bytes.ReplaceAll(payload, []byte(claudeRefusalRetryNudge), []byte(claudeRefusalReplacement))
	payload = bytes.ReplaceAll(payload, []byte(strings.ReplaceAll(claudeRefusalRetryNudge, "—", "\\u2014")), []byte(claudeRefusalReplacement))
	var body map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil {
		return nil, fmt.Errorf("decode group request body: %w", errUnmarshal)
	}
	if rawMessages, ok := body["messages"]; ok {
		body["messages"] = stripNoticeFromMessages(rawMessages)
	}
	if _, ok := body["model"]; !ok {
		return json.Marshal(body)
	}
	encoded, _ := json.Marshal(model)
	body["model"] = encoded
	return json.Marshal(body)
}

func groupFailure(group modelGroup, attempts []groupAttempt, lastErr error) error {
	parts := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		parts = append(parts, attempt.Model+": "+attempt.Result)
	}
	status := http.StatusServiceUnavailable
	var errStatus *statusError
	if errors.As(lastErr, &errStatus) && errStatus.HTTPStatus != 0 {
		status = errStatus.HTTPStatus
	}
	return &statusError{Code: "model_group_exhausted", Message: fmt.Sprintf("model group %s: every model failed (%s)", group.ID, strings.Join(parts, "; ")), HTTPStatus: status}
}

func recordGroupDecision(group modelGroup, attempts []groupAttempt, served string) {
	reason := "every model failed"
	if served != "" {
		reason = "served by " + served
	}
	stateMu.Lock()
	recordDecision(decision{Time: time.Now(), Kind: "group", Model: group.ID, AuthID: served, Reason: reason, Attempts: attempts})
	stateMu.Unlock()
}
