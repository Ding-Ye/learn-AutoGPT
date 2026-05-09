package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// provider_anthropic_test.go — s03 RENAMED this from `provider_test.go` to
// make the file's scope explicit: it tests ONLY the Anthropic-native path.
// The OpenAI-compat translation tests live in `provider_openai_test.go`,
// the mock provider tests in `provider_mock_test.go`. With three Provider
// implementations co-existing, naming each test file after its target
// avoids the "wait, which provider does this exercise?" wobble that
// `provider_test.go` had in s01/s02.

// TestAnthropicProvider_RoundTrip — wire-format test. Spin up an
// httptest.Server, point the provider at it via the `baseURL` field, and
// assert that (a) the outgoing request is shaped exactly like Anthropic
// expects (path, headers, JSON body), and (b) we decode the canned
// response into our internal CreateMessageResponse correctly.
func TestAnthropicProvider_RoundTrip(t *testing.T) {
	var capturedPath string
	var capturedHeaders http.Header
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "msg_1",
			"role": "assistant",
			"content": [{"type": "text", "text": "ok"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		apiKey:  "test",
		model:   "claude-test",
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	resp, err := p.CreateMessage(context.Background(), CreateMessageRequest{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// --- request shape
	if capturedPath != "/v1/messages" {
		t.Errorf("path = %q, want %q", capturedPath, "/v1/messages")
	}
	if got := capturedHeaders.Get("x-api-key"); got != "test" {
		t.Errorf("x-api-key = %q, want %q", got, "test")
	}
	if got := capturedHeaders.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want %q", got, "2023-06-01")
	}
	if got := capturedHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	// Confirm body is valid JSON and contains the model we configured.
	var parsedBody map[string]interface{}
	if err := json.Unmarshal(capturedBody, &parsedBody); err != nil {
		t.Errorf("body is not valid JSON: %v (body=%s)", err, capturedBody)
	}
	if parsedBody["model"] != "claude-test" {
		t.Errorf("body.model = %v, want %q", parsedBody["model"], "claude-test")
	}

	// --- response shape
	if len(resp.Content) != 1 || resp.Content[0].Text != "ok" {
		t.Errorf("Content = %+v, want [{text:ok}]", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "end_turn")
	}
}

// TestAnthropicProvider_ErrorsOn401 — non-2xx responses must surface as
// errors that include the status so the operator sees "401" rather than a
// silent empty response.
func TestAnthropicProvider_ErrorsOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		apiKey:  "bad",
		model:   "claude-test",
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	_, err := p.CreateMessage(context.Background(), CreateMessageRequest{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q must mention status 401", err.Error())
	}
}

// TestAnthropicProvider_DefaultsModelAndMaxTokens — when the request
// leaves Model="" or MaxTokens=0, the provider should fill from its
// configured defaults. This protects callers (Loop, tests) from having
// to repeat the same model name on every call.
func TestAnthropicProvider_DefaultsModelAndMaxTokens(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "msg_x",
			"role": "assistant",
			"content": [{"type": "text", "text": "hi"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		apiKey:  "k",
		model:   "claude-default-on-provider",
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	// Note: empty Model and MaxTokens — the provider must inject defaults.
	if _, err := p.CreateMessage(context.Background(), CreateMessageRequest{
		Messages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	}); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["model"] != "claude-default-on-provider" {
		t.Errorf("default model not applied: %v", body["model"])
	}
	// max_tokens defaulted to 4096 — JSON-decoded as float64.
	if mt, ok := body["max_tokens"].(float64); !ok || mt != 4096 {
		t.Errorf("default max_tokens not applied: %v", body["max_tokens"])
	}
}
