package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIProvider works with ANY OpenAI-compatible Chat Completions API.
// We keep the agent loop's INTERNAL types Anthropic-style (Message,
// ContentBlock with type=tool_use/tool_result, stop_reason). This
// provider does the wire-format translation in both directions.
type OpenAIProvider struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// NewOpenAIProvider builds a provider against the given base URL.
func NewOpenAIProvider(apiKey, baseURL, model string) *OpenAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (o *OpenAIProvider) CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error) {
	model := req.Model
	if model == "" {
		model = o.model
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	oaiReq := translateRequestToOpenAI(req, model, maxTokens)
	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("encode openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("openai-compat API %d: %s", resp.StatusCode, string(respBody))
	}

	var oaiResp openAIChatCompletionResp
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("decode openai response: %w (body=%s)", err, string(respBody))
	}
	return translateResponseFromOpenAI(&oaiResp), nil
}

// ---------------------------------------------------------------- wire types

type openAIChatRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	Tools     []openAITool    `json:"tools,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolCallFunc `json:"function"`
}

type openAIToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string        `json:"type"`
	Function openAIToolDef `json:"function"`
}

type openAIToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type openAIChatCompletionResp struct {
	Choices []openAIChatChoice `json:"choices"`
	Usage   openAIUsage        `json:"usage"`
}

type openAIChatChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// --------------------------------------------------- translation: out (req)

func translateRequestToOpenAI(req CreateMessageRequest, model string, maxTokens int) openAIChatRequest {
	out := openAIChatRequest{
		Model:     model,
		MaxTokens: maxTokens,
	}
	if req.System != "" {
		out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, anthropicMessageToOpenAI(m)...)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, openAITool{
			Type: "function",
			Function: openAIToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

func anthropicMessageToOpenAI(m Message) []openAIMessage {
	var out []openAIMessage
	switch m.Role {
	case "user":
		var texts []string
		var tools []openAIMessage
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
			case "tool_result":
				content := stringifyToolResult(b.ToolContent)
				tools = append(tools, openAIMessage{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    content,
				})
			}
		}
		if len(texts) > 0 {
			out = append(out, openAIMessage{Role: "user", Content: strings.Join(texts, "\n")})
		}
		out = append(out, tools...)

	case "assistant":
		var texts []string
		var calls []openAIToolCall
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
			case "tool_use":
				args, _ := json.Marshal(b.Input)
				calls = append(calls, openAIToolCall{
					ID:   b.ID,
					Type: "function",
					Function: openAIToolCallFunc{
						Name:      b.Name,
						Arguments: string(args),
					},
				})
			}
		}
		msg := openAIMessage{Role: "assistant"}
		if len(texts) > 0 {
			msg.Content = strings.Join(texts, "\n")
		}
		if len(calls) > 0 {
			msg.ToolCalls = calls
		}
		out = append(out, msg)
	}
	return out
}

func stringifyToolResult(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	if data, err := json.Marshal(v); err == nil {
		return string(data)
	}
	return fmt.Sprintf("%v", v)
}

// --------------------------------------------------- translation: in (resp)

func translateResponseFromOpenAI(resp *openAIChatCompletionResp) *CreateMessageResponse {
	out := &CreateMessageResponse{
		Usage: Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
	if len(resp.Choices) == 0 {
		out.StopReason = "end_turn"
		return out
	}
	choice := resp.Choices[0]
	if text, ok := contentToString(choice.Message.Content); ok && strings.TrimSpace(text) != "" {
		out.Content = append(out.Content, ContentBlock{Type: "text", Text: text})
	}
	for _, tc := range choice.Message.ToolCalls {
		var input map[string]interface{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
			input = map[string]interface{}{"_raw_arguments": tc.Function.Arguments}
		}
		out.Content = append(out.Content, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}
	switch choice.FinishReason {
	case "stop", "":
		out.StopReason = "end_turn"
	case "tool_calls", "function_call":
		out.StopReason = "tool_use"
	case "length":
		out.StopReason = "max_tokens"
	default:
		out.StopReason = "end_turn"
	}
	return out
}

func contentToString(v interface{}) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case nil:
		return "", false
	case []interface{}:
		var parts []string
		for _, item := range x {
			if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					if txt, ok := m["text"].(string); ok {
						parts = append(parts, txt)
					}
				}
			}
		}
		return strings.Join(parts, ""), len(parts) > 0
	}
	return "", false
}
