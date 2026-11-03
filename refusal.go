package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// streamPrefetchBytes is how many bytes of visible text executeGroupStream buffers before
	// committing to a candidate model, so prompt-level, thinking-phase, and early streaming
	// safeguard refusals can switch to the next model in the group before any chunk is sent.
	streamPrefetchBytes = 512

	claudeRefusalRetryNudge  = "Your response above was stopped by a safety classifier — this is not a tool or API error. The rest of it was withheld, and tool calls in it that had not finished did not run. Do not produce that content again, even reworded."
	claudeRefusalReplacement = "Your previous response was interrupted mid-stream. Continue directly from where it stopped."

	noticePrefix = "⚡ [Quota Smart Router] "
)

// sessionRefusals tracks models that refused in a session, keyed by session + "\x00" + groupID -> model -> time.
var sessionRefusals = make(map[string]map[string]time.Time)

// sessionGroupModels remembers the last model that served a group in a session so the client
// is notified whenever the active model changes.
var sessionGroupModels = make(map[string]string)

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

func updateSessionGroupModel(session, groupID, served string) string {
	if session == "" {
		return ""
	}
	key := session + "\x00" + groupID
	stateMu.Lock()
	prev := sessionGroupModels[key]
	sessionGroupModels[key] = served
	stateMu.Unlock()
	return prev
}

// buildGroupNotice formats a user-visible line when a model in the group failed/refused and CPA
// recovered on a fallback model, or when the active model for the session changed.
func buildGroupNotice(group modelGroup, attempts []groupAttempt, served, prevModel string) string {
	hadActiveFailure := false
	var prior []string
	for _, attempt := range attempts {
		if attempt.Model == served && attempt.Result == "served" {
			break
		}
		if strings.HasPrefix(attempt.Result, "failed: ") {
			hadActiveFailure = true
		}
		detail := strings.TrimPrefix(attempt.Result, "failed: ")
		detail = strings.TrimPrefix(detail, "skipped: ")
		prior = append(prior, fmt.Sprintf("%s (%s)", attempt.Model, detail))
	}
	modelChanged := (prevModel != "" && prevModel != served) ||
		(prevModel == "" && len(group.Models) > 0 && served != group.Models[0])
	if !hadActiveFailure && !modelChanged {
		return ""
	}
	if len(prior) > 0 {
		return noticePrefix + strings.Join(prior, " → ") + " → switched to " + served
	}
	return fmt.Sprintf("%sswitched from %s → %s", noticePrefix, prevModel, served)
}

func effectiveFormat(exitFormat, sourceFormat string) string {
	if exitFormat != "" {
		return strings.ToLower(strings.TrimSpace(exitFormat))
	}
	return strings.ToLower(strings.TrimSpace(sourceFormat))
}

// injectNonStreamNotice prepends notice to a non-streaming response body.
func injectNonStreamNotice(body []byte, format, notice string) []byte {
	if notice == "" {
		return body
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	if rawContent, ok := root["content"]; ok {
		var blocks []json.RawMessage
		if json.Unmarshal(rawContent, &blocks) == nil {
			noticeBlock, _ := json.Marshal(map[string]string{"type": "text", "text": notice})
			root["content"], _ = json.Marshal(append([]json.RawMessage{noticeBlock}, blocks...))
			if out, errMarshal := json.Marshal(root); errMarshal == nil {
				return out
			}
		}
	}
	if rawChoices, ok := root["choices"]; ok {
		var choices []map[string]json.RawMessage
		if json.Unmarshal(rawChoices, &choices) == nil && len(choices) > 0 {
			var msg map[string]json.RawMessage
			if json.Unmarshal(choices[0]["message"], &msg) == nil {
				var content string
				if json.Unmarshal(msg["content"], &content) == nil {
					msg["content"], _ = json.Marshal(notice + "\n\n" + content)
					choices[0]["message"], _ = json.Marshal(msg)
					root["choices"], _ = json.Marshal(choices)
					if out, errMarshal := json.Marshal(root); errMarshal == nil {
						return out
					}
				}
			}
		}
	}
	return body
}

// injectStreamNotice injects notice at the start of prefetched stream chunks. It returns the
// updated chunks and whether subsequent chunks in forwardStream must shift Claude content_block
// indices by +1.
func injectStreamNotice(prefetched [][]byte, format, notice string) ([][]byte, bool) {
	if notice == "" || len(prefetched) == 0 {
		return prefetched, false
	}
	if format == "claude" || bytes.Contains(prefetched[0], []byte(`"message_start"`)) {
		textJSON, _ := json.Marshal(notice)
		noticeSSE := fmt.Sprintf(
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n"+
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			string(textJSON),
		)
		out := make([][]byte, 0, len(prefetched)+1)
		inserted := false
		for _, chunk := range prefetched {
			shifted, afterStart := shiftClaudeStreamChunk(chunk, !inserted, noticeSSE)
			if afterStart {
				inserted = true
			}
			out = append(out, shifted)
		}
		if !inserted {
			out = append([][]byte{[]byte(noticeSSE)}, out...)
		}
		return out, true
	}
	// OpenAI chat SSE: emit a leading delta chunk with the notice.
	textJSON, _ := json.Marshal(notice + "\n\n")
	head := fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%s}}]}\n\n", string(textJSON))
	return append([][]byte{[]byte(head)}, prefetched...), false
}

// shiftClaudeStreamChunk increments top-level content_block_* indices by +1 and optionally
// inserts noticeSSE right after the message_start event.
func shiftClaudeStreamChunk(chunk []byte, insertAfterMessageStart bool, noticeSSE string) ([]byte, bool) {
	lines := bytes.Split(chunk, []byte("\n"))
	var out bytes.Buffer
	inserted := false
	afterMessageStartData := false
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if len(data) > 0 && data[0] == '{' {
				var obj map[string]json.RawMessage
				if json.Unmarshal(data, &obj) == nil {
					var typ string
					_ = json.Unmarshal(obj["type"], &typ)
					switch typ {
					case "message_start":
						if insertAfterMessageStart && !inserted {
							afterMessageStartData = true
						}
					case "content_block_start", "content_block_delta", "content_block_stop":
						var idx int
						if json.Unmarshal(obj["index"], &idx) == nil {
							obj["index"] = json.RawMessage(strconv.Itoa(idx + 1))
							if updated, errMarshal := json.Marshal(obj); errMarshal == nil {
								line = append([]byte("data: "), updated...)
							}
						}
					}
				}
			}
		}
		out.Write(line)
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
		if afterMessageStartData && len(trimmed) == 0 && !inserted {
			out.WriteString(noticeSSE)
			inserted = true
			afterMessageStartData = false
		}
	}
	if afterMessageStartData && !inserted {
		if !bytes.HasSuffix(out.Bytes(), []byte("\n\n")) {
			out.WriteByte('\n')
		}
		out.WriteString(noticeSSE)
		inserted = true
	}
	return out.Bytes(), inserted
}

// stripNoticeFromMessages removes previously injected Quota Smart Router notice blocks from
// conversation history so upstream models never see them.
func stripNoticeFromMessages(rawMessages json.RawMessage) json.RawMessage {
	if !bytes.Contains(rawMessages, []byte(noticePrefix)) {
		return rawMessages
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(rawMessages, &messages) != nil {
		return rawMessages
	}
	for i, msg := range messages {
		rawContent, ok := msg["content"]
		if !ok || !bytes.Contains(rawContent, []byte(noticePrefix)) {
			continue
		}
		var text string
		if json.Unmarshal(rawContent, &text) == nil {
			if strings.HasPrefix(text, noticePrefix) {
				if idx := strings.Index(text, "\n\n"); idx != -1 {
					msg["content"], _ = json.Marshal(text[idx+2:])
					messages[i] = msg
				}
			}
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(rawContent, &blocks) != nil {
			continue
		}
		filtered := make([]map[string]json.RawMessage, 0, len(blocks))
		for _, block := range blocks {
			var blockText string
			if json.Unmarshal(block["text"], &blockText) == nil && strings.HasPrefix(strings.TrimSpace(blockText), noticePrefix) {
				continue
			}
			filtered = append(filtered, block)
		}
		if len(filtered) > 0 && len(filtered) != len(blocks) {
			msg["content"], _ = json.Marshal(filtered)
			messages[i] = msg
		}
	}
	out, errMarshal := json.Marshal(messages)
	if errMarshal != nil {
		return rawMessages
	}
	return out
}

type requestInterceptRequest struct {
	Body []byte `json:"Body"`
}

type requestInterceptResponse struct {
	Body []byte `json:"Body,omitempty"`
}

// handleRequestIntercept strips Quota Smart Router notice blocks from all incoming requests
// (including when the user switches away from a group model mid-session) so they never enter
// any model's context.
func handleRequestIntercept(raw []byte) ([]byte, error) {
	var req requestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if !bytes.Contains(req.Body, []byte(noticePrefix)) {
		return okEnvelope(requestInterceptResponse{})
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(req.Body, &body) != nil {
		return okEnvelope(requestInterceptResponse{})
	}
	rawMessages, ok := body["messages"]
	if !ok {
		return okEnvelope(requestInterceptResponse{})
	}
	cleaned := stripNoticeFromMessages(rawMessages)
	if bytes.Equal(cleaned, rawMessages) {
		return okEnvelope(requestInterceptResponse{})
	}
	body["messages"] = cleaned
	updated, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return okEnvelope(requestInterceptResponse{})
	}
	return okEnvelope(requestInterceptResponse{Body: updated})
}
