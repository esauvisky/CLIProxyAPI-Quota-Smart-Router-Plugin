package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// streamPrefetchBytes is how many bytes of visible text executeGroupStream buffers before
	// committing to a candidate model, so prompt-level, thinking-phase, and early streaming
	// safeguard refusals can switch to the next model in the group before any chunk is sent.
	streamPrefetchBytes = 512

	claudeRefusalRetryNudge = "Your response above was stopped by a safety classifier — this is not a tool or API error. The rest of it was withheld, and tool calls in it that had not finished did not run. Do not produce that content again, even reworded."
	claudeRefusalReplacement = "Your previous response was interrupted mid-stream. Continue directly from where it stopped."
)

// sessionRefusals tracks models that refused mid-stream (after visible output was already
// flushed to the client), keyed by session + "\x00" + groupID -> model -> time.
var sessionRefusals = make(map[string]map[string]time.Time)

func recordSessionRefusal(session, groupID, model string, now time.Time) {
	if session == "" {
		return
	}
	key := session + "\x00" + groupID
	stateMu.Lock()
	byModel := sessionRefusals[key]
	if byModel == nil {
		byModel = make(map[string]time.Time)
		sessionRefusals[key] = byModel
	}
	byModel[model] = now
	stateMu.Unlock()
}

func isSessionRefused(cfg pluginConfig, session, groupID, model string, now time.Time) bool {
	if session == "" {
		return false
	}
	ttl := time.Duration(cfg.IdleMinutes) * time.Minute
	key := session + "\x00" + groupID
	stateMu.Lock()
	defer stateMu.Unlock()
	at, ok := sessionRefusals[key][model]
	if !ok {
		return false
	}
	if now.Sub(at) >= ttl {
		delete(sessionRefusals[key], model)
		return false
	}
	return true
}

func clearSessionRefusals(session, groupID string) {
	if session == "" {
		return
	}
	stateMu.Lock()
	delete(sessionRefusals, session+"\x00"+groupID)
	stateMu.Unlock()
}

// resetRefusalsIfAllRefused clears session refusal marks when every model in the group has
// refused, so the next turn tries the group from the top instead of skipping every model.
func resetRefusalsIfAllRefused(cfg pluginConfig, session string, group modelGroup, now time.Time) {
	if session == "" || len(group.Models) == 0 {
		return
	}
	for _, model := range group.Models {
		if !isSessionRefused(cfg, session, group.ID, model, now) {
			return
		}
	}
	clearSessionRefusals(session, group.ID)
}

// inspectPayload parses a non-streaming response body or one JSON event from an SSE chunk and
// returns any refusal/error reason, the number of visible text bytes, whether a tool call is
// present, and whether the JSON matches a known model response envelope.
func inspectPayload(raw []byte) (refusal string, visibleBytes int, sawToolUse bool, sawEnvelope bool) {
	var msg struct {
		Type  string `json:"type"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		StopReason  string `json:"stop_reason"`
		StopDetails *struct {
			Category string `json:"category"`
		} `json:"stop_details"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Refusal     string `json:"refusal"`
			StopReason  string `json:"stop_reason"`
			StopDetails *struct {
				Category string `json:"category"`
			} `json:"stop_details"`
		} `json:"delta"`
		ContentBlock *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_block"`
		Content json.RawMessage `json:"content"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      *struct {
				Content      json.RawMessage `json:"content"`
				Refusal      string          `json:"refusal"`
				ToolCalls    []any           `json:"tool_calls"`
				FunctionCall any             `json:"function_call"`
			} `json:"message"`
			Delta *struct {
				Content      string `json:"content"`
				Refusal      string `json:"refusal"`
				ToolCalls    []any  `json:"tool_calls"`
				FunctionCall any    `json:"function_call"`
			} `json:"delta"`
		} `json:"choices"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Response *struct {
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			PromptFeedback *struct {
				BlockReason string `json:"blockReason"`
			} `json:"promptFeedback"`
			Candidates [] geminiCandidate `json:"candidates"`
		} `json:"response"`
		Item *struct {
			Type string `json:"type"`
		} `json:"item"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
		PromptFeedback *struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Candidates []geminiCandidate `json:"candidates"`
	}
	if errUnmarshal := json.Unmarshal(raw, &msg); errUnmarshal != nil {
		return "", 0, false, false
	}
	sawEnvelope = msg.StopReason != "" || msg.Delta != nil || msg.ContentBlock != nil ||
		len(msg.Content) > 0 || len(msg.Choices) > 0 || len(msg.Candidates) > 0 ||
		msg.Response != nil || len(msg.Output) > 0 ||
		strings.HasPrefix(msg.Type, "message_") || strings.HasPrefix(msg.Type, "content_block_") || strings.HasPrefix(msg.Type, "response.")

	if msg.Type == "error" || (msg.Error != nil && (msg.Error.Message != "" || msg.Error.Type != "")) {
		if msg.Error != nil && msg.Error.Message != "" {
			return "upstream error: " + msg.Error.Message, 0, false, true
		}
		return "upstream error", 0, false, true
	}
	if reason := claudeStopRefusal(msg.StopReason, msg.StopDetails); reason != "" {
		return reason, 0, false, true
	}
	if msg.Delta != nil {
		if reason := claudeStopRefusal(msg.Delta.StopReason, msg.Delta.StopDetails); reason != "" {
			return reason, 0, false, true
		}
		if msg.Delta.Refusal != "" {
			return "refusal", 0, false, true
		}
		switch msg.Delta.Type {
		case "text_delta":
			visibleBytes += len(msg.Delta.Text)
		case "input_json_delta":
			if msg.Delta.PartialJSON != "" {
				sawToolUse = true
			}
		}
	}
	if msg.ContentBlock != nil {
		switch msg.ContentBlock.Type {
		case "tool_use", "server_tool_use":
			sawToolUse = true
		case "text":
			visibleBytes += len(msg.ContentBlock.Text)
		}
	}
	if len(msg.Content) > 0 {
		ref, vis, tool := inspectClaudeContent(msg.Content)
		if ref != "" {
			return ref, 0, false, true
		}
		visibleBytes += vis
		sawToolUse = sawToolUse || tool
	}
	if msg.IncompleteDetails != nil && msg.IncompleteDetails.Reason == "content_filter" {
		return "refusal (content_filter)", 0, false, true
	}
	if msg.Response != nil && msg.Response.IncompleteDetails != nil && msg.Response.IncompleteDetails.Reason == "content_filter" {
		return "refusal (content_filter)", 0, false, true
	}
	if msg.Type == "response.output_text.delta" {
		var deltaEvt struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(raw, &deltaEvt) == nil {
			visibleBytes += len(deltaEvt.Delta)
		}
	}
	if msg.Type == "response.function_call_arguments.delta" || (msg.Item != nil && msg.Item.Type == "function_call") {
		sawToolUse = true
	}
	for _, item := range msg.Output {
		if item.Type == "function_call" {
			sawToolUse = true
		}
		for _, part := range item.Content {
			if part.Type == "refusal" || part.Refusal != "" {
				return "refusal", 0, false, true
			}
			visibleBytes += len(strings.TrimSpace(part.Text))
		}
	}
	for _, choice := range msg.Choices {
		if choice.FinishReason == "content_filter" {
			return "refusal (content_filter)", 0, false, true
		}
		if choice.Message != nil {
			if choice.Message.Refusal != "" {
				return "refusal", 0, false, true
			}
			if len(choice.Message.ToolCalls) > 0 || choice.Message.FunctionCall != nil {
				sawToolUse = true
			}
			visibleBytes += inspectOpenAIMessageContent(choice.Message.Content)
		}
		if choice.Delta != nil {
			if choice.Delta.Refusal != "" {
				return "refusal", 0, false, true
			}
			if len(choice.Delta.ToolCalls) > 0 || choice.Delta.FunctionCall != nil {
				sawToolUse = true
			}
			visibleBytes += len(choice.Delta.Content)
		}
	}
	if msg.PromptFeedback != nil && msg.PromptFeedback.BlockReason != "" {
		return "refusal (" + msg.PromptFeedback.BlockReason + ")", 0, false, true
	}
	if msg.Response != nil && msg.Response.PromptFeedback != nil && msg.Response.PromptFeedback.BlockReason != "" {
		return "refusal (" + msg.Response.PromptFeedback.BlockReason + ")", 0, false, true
	}
	candidates := msg.Candidates
	if msg.Response != nil && len(msg.Response.Candidates) > 0 {
		candidates = append(candidates, msg.Response.Candidates...)
	}
	for _, cand := range candidates {
		if isGeminiRefusalFinishReason(cand.FinishReason) {
			return "refusal (" + cand.FinishReason + ")", 0, false, true
		}
		for _, part := range cand.Content.Parts {
			if part.FunctionCall != nil {
				sawToolUse = true
			}
			if !part.Thought {
				visibleBytes += len(part.Text)
			}
		}
	}
	return "", visibleBytes, sawToolUse, sawEnvelope
}

type geminiCandidate struct {
	FinishReason string `json:"finishReason"`
	Content      struct {
		Parts []struct {
			Text         string `json:"text"`
			Thought      bool   `json:"thought"`
			FunctionCall any    `json:"functionCall"`
		} `json:"parts"`
	} `json:"content"`
}

func claudeStopRefusal(stopReason string, details *struct {
	Category string `json:"category"`
}) string {
	if stopReason != "refusal" && stopReason != "sensitive" {
		return ""
	}
	if details != nil && details.Category != "" {
		return "refusal (" + details.Category + ")"
	}
	return "refusal"
}

func inspectClaudeContent(raw json.RawMessage) (string, int, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return "", len(strings.TrimSpace(text)), false
	}
	var blocks []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", 0, false
	}
	visible := 0
	tool := false
	for _, block := range blocks {
		switch block.Type {
		case "refusal":
			return "refusal", 0, false
		case "tool_use", "server_tool_use":
			tool = true
		case "text":
			visible += len(strings.TrimSpace(block.Text))
		}
	}
	return "", visible, tool
}

func inspectOpenAIMessageContent(raw json.RawMessage) int {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return len(strings.TrimSpace(text))
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return 0
	}
	total := 0
	for _, part := range parts {
		if part.Type != "thinking" && part.Type != "reasoning" {
			total += len(strings.TrimSpace(part.Text))
		}
	}
	return total
}

func isGeminiRefusalFinishReason(reason string) bool {
	switch reason {
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "SPII", "BLOCKLIST", "IMAGE_SAFETY":
		return true
	default:
		return false
	}
}

// inspectStreamChunk inspects all JSON objects in an SSE or raw-JSON stream chunk.
func inspectStreamChunk(payload []byte) (refusal string, visibleBytes int, sawToolUse bool, sawEnvelope bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return "", 0, false, false
	}
	if trimmed[0] == '{' {
		return inspectPayload(trimmed)
	}
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || data[0] != '{' {
			continue
		}
		ref, vis, tool, env := inspectPayload(data)
		if ref != "" {
			return ref, 0, false, true
		}
		visibleBytes += vis
		sawToolUse = sawToolUse || tool
		sawEnvelope = sawEnvelope || env
	}
	return "", visibleBytes, sawToolUse, sawEnvelope
}

// checkNonStreamResponse returns an error if a non-streaming response is a refusal or has no
// visible text or tool calls.
func checkNonStreamResponse(body []byte) error {
	refusal, visible, sawTool, sawEnvelope := inspectPayload(bytes.TrimSpace(body))
	if refusal != "" {
		return fmt.Errorf("%s", refusal)
	}
	if sawEnvelope && visible == 0 && !sawTool {
		return fmt.Errorf("empty response (no text or tool calls)")
	}
	return nil
}

// prefetchStream reads initial chunks from sourceStreamID until at least streamPrefetchBytes of
// visible text (or a tool call) arrive, or the stream finishes. If a refusal or error occurs
// before that threshold, sourceStreamID is closed and an error is returned so the caller can
// switch to the next model in the group before emitting anything to the client.
func prefetchStream(sourceStreamID, callbackID string) ([][]byte, bool, error) {
	var prefetched [][]byte
	totalVisible := 0
	sawTool := false
	sawEnvelope := false
	for {
		rawRead, errRead := hostCall(methodHostModelStreamRead, hostStreamRef{StreamID: sourceStreamID, HostCallbackID: callbackID})
		if errRead != nil {
			_, _ = hostCall(methodHostModelStreamClose, hostStreamRef{StreamID: sourceStreamID, HostCallbackID: callbackID})
			return nil, false, errRead
		}
		var chunk hostStreamReadResponse
		if errDecode := json.Unmarshal(rawRead, &chunk); errDecode != nil {
			_, _ = hostCall(methodHostModelStreamClose, hostStreamRef{StreamID: sourceStreamID, HostCallbackID: callbackID})
			return nil, false, fmt.Errorf("decode host.model.stream_read response: %w", errDecode)
		}
		if len(chunk.Payload) > 0 {
			refusal, vis, tool, env := inspectStreamChunk(chunk.Payload)
			if refusal != "" {
				_, _ = hostCall(methodHostModelStreamClose, hostStreamRef{StreamID: sourceStreamID, HostCallbackID: callbackID})
				return nil, false, fmt.Errorf("%s", refusal)
			}
			totalVisible += vis
			sawTool = sawTool || tool
			sawEnvelope = sawEnvelope || env
			prefetched = append(prefetched, chunk.Payload)
		}
		if chunk.Error != "" {
			_, _ = hostCall(methodHostModelStreamClose, hostStreamRef{StreamID: sourceStreamID, HostCallbackID: callbackID})
			return nil, false, fmt.Errorf("%s", chunk.Error)
		}
		if chunk.Done {
			if sawEnvelope && totalVisible == 0 && !sawTool {
				_, _ = hostCall(methodHostModelStreamClose, hostStreamRef{StreamID: sourceStreamID, HostCallbackID: callbackID})
				return nil, false, fmt.Errorf("empty response (no text or tool calls)")
			}
			return prefetched, true, nil
		}
		if sawTool || totalVisible >= streamPrefetchBytes {
			return prefetched, false, nil
		}
	}
}
