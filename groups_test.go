package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func groupConfig() pluginConfig {
	cfg := balanceConfig()
	cfg.Groups = []modelGroup{{ID: "uncensored-models", Name: "Uncensored", Models: []string{"glm-5.3-uncensored", "deepseek-v4.1-flash-uncensored", "abliterated"}}}
	return cfg
}

func decodeOK(t *testing.T, raw []byte, out any) {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("envelope = %s (%v)", raw, errUnmarshal)
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		t.Fatalf("decode result: %v", errUnmarshal)
	}
}

func TestRouteGroupOnlyClaimsGroupIDs(t *testing.T) {
	var resp modelRouteResponse
	raw, _ := routeGroup([]byte(`{"RequestedModel":"uncensored-models"}`), groupConfig())
	decodeOK(t, raw, &resp)
	if !resp.Handled || resp.TargetKind != "self" {
		t.Fatalf("group route = %+v", resp)
	}
	raw, _ = routeGroup([]byte(`{"RequestedModel":"claude-opus-4-8"}`), groupConfig())
	resp = modelRouteResponse{}
	decodeOK(t, raw, &resp)
	if resp.Handled {
		t.Fatalf("plain model route handled: %+v", resp)
	}
}

func TestGroupModelRegistration(t *testing.T) {
	reg := groupModelRegistration(groupConfig())
	if reg.Provider != groupProvider || len(reg.Models) != 1 || reg.Models[0].ID != "uncensored-models" || reg.Models[0].DisplayName != "Uncensored" {
		t.Fatalf("registration = %+v", reg)
	}
}

func TestExecuteGroupFallsThroughFailuresAndLowQuota(t *testing.T) {
	var tried []string
	fakeHost(t, []string{"glm-5.3-uncensored", "deepseek-v4.1-flash-uncensored", "abliterated"}, func(method string, payload any) (json.RawMessage, error) {
		if method != methodHostModelExecute {
			t.Fatalf("unexpected host call %s", method)
		}
		req := payload.(hostModelRequest)
		tried = append(tried, req.Model)
		var body map[string]string
		_ = json.Unmarshal(req.Body, &body)
		if body["model"] != req.Model || req.EntryProtocol != "openai" || req.HostCallbackID != "cb" {
			t.Fatalf("host request = %+v body %s", req, req.Body)
		}
		if req.Model == "glm-5.3-uncensored" {
			return nil, &statusError{Code: "host_call_failed", Message: "upstream 500", HTTPStatus: 500}
		}
		return json.Marshal(hostModelResponse{StatusCode: 200, Body: []byte(`{"served":"` + req.Model + `"}`)})
	}, "only-cred")
	// The credential's deepseek-only window is drained, so deepseek is skipped without a call.
	// skipReason runs on the real clock, so the windows are anchored to it.
	now := time.Now()
	setQuota(t, "only-cred",
		quotaWindow{Name: "7d", Remaining: 0.5, ResetAt: now.Add(24 * time.Hour), Period: 7 * 24 * time.Hour},
		quotaWindow{Name: "deepseek weekly", Remaining: 0, ResetAt: now.Add(24 * time.Hour), Period: 7 * 24 * time.Hour, Models: []string{"deepseek-v4.1-flash-uncensored"}})

	raw, errExec := executeGroup([]byte(`{"Model":"uncensored-models","SourceFormat":"openai","Format":"openai","Payload":"`+b64(`{"model":"uncensored-models","messages":[]}`)+`","host_callback_id":"cb"}`), groupConfig())
	if errExec != nil {
		t.Fatalf("executeGroup() error = %v", errExec)
	}
	var resp executorResponse
	decodeOK(t, raw, &resp)
	if string(resp.Payload) != `{"served":"abliterated"}` || strings.Join(tried, ",") != "glm-5.3-uncensored,abliterated" {
		t.Fatalf("payload = %s tried = %v", resp.Payload, tried)
	}
	stateMu.Lock()
	last := decisions[len(decisions)-1]
	stateMu.Unlock()
	if last.Kind != "group" || len(last.Attempts) != 3 || !strings.HasPrefix(last.Attempts[1].Result, "skipped") {
		t.Fatalf("decision = %+v", last)
	}
}

func TestExecuteGroupAllFailReturnsLastStatus(t *testing.T) {
	fakeHost(t, nil, func(method string, payload any) (json.RawMessage, error) {
		return nil, &statusError{Code: "host_call_failed", Message: "rate limited", HTTPStatus: 429}
	})
	_, errExec := executeGroup([]byte(`{"Model":"uncensored-models","SourceFormat":"openai","Payload":"`+b64(`{"model":"x"}`)+`"}`), groupConfig())
	var errStatus *statusError
	if !errors.As(errExec, &errStatus) || errStatus.HTTPStatus != 429 || !strings.Contains(errStatus.Message, "abliterated: failed") {
		t.Fatalf("executeGroup() error = %v", errExec)
	}
}

func TestExecuteGroupStreamForwardsChunks(t *testing.T) {
	emitted := make(chan string, 8)
	reads := 0
	fakeHost(t, nil, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case methodHostModelStream:
			if payload.(hostModelRequest).Model == "glm-5.3-uncensored" {
				return nil, &statusError{Message: "no auth", HTTPStatus: 503}
			}
			return json.Marshal(hostModelStreamResponse{StatusCode: 200, StreamID: "src"})
		case methodHostModelStreamRead:
			reads++
			if reads == 1 {
				return json.Marshal(hostStreamReadResponse{Payload: []byte("data: one\n\n")})
			}
			return json.Marshal(hostStreamReadResponse{Done: true})
		case methodHostStreamEmit:
			emitted <- "emit:" + string(payload.(hostStreamEmit).Payload)
			return json.RawMessage(`{}`), nil
		case methodHostStreamClose:
			emitted <- "close:" + payload.(hostStreamEmit).StreamID
			return json.RawMessage(`{}`), nil
		}
		t.Errorf("unexpected host call %s", method)
		return nil, nil
	})
	raw, errExec := executeGroupStream([]byte(`{"Model":"uncensored-models","SourceFormat":"claude","Stream":true,"Payload":"`+b64(`{"model":"x","stream":true}`)+`","stream_id":"dst"}`), groupConfig())
	if errExec != nil {
		t.Fatalf("executeGroupStream() error = %v", errExec)
	}
	var open executorStreamOpenResponse
	decodeOK(t, raw, &open)
	if got := <-emitted; got != "emit:data: one\n\n" {
		t.Fatalf("first event = %q", got)
	}
	if got := <-emitted; got != "close:dst" {
		t.Fatalf("second event = %q", got)
	}
}

func TestWithModelKeepsBodiesWithoutModelField(t *testing.T) {
	out, errSet := withModel([]byte(`{"contents":[]}`), "m")
	if errSet != nil || string(out) != `{"contents":[]}` {
		t.Fatalf("withModel = %s, %v", out, errSet)
	}
	if _, errSet := withModel([]byte(`not json`), "m"); errSet == nil {
		t.Fatalf("withModel(non-JSON) error = nil")
	}
}

func b64(s string) string {
	raw, _ := json.Marshal([]byte(s))
	return strings.Trim(string(raw), `"`)
}
