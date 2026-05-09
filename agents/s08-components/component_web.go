// component_web.go — WebFetchComponent.
//
// WebFetchComponent is the second example: a minimum-viable component
// that exposes ONE network capability — `web_fetch` — to the agent.
// It implements only `CommandProvider` (no directives, no messages),
// proving that components can be "narrow capability bundles" too.
//
// The tool itself does a plain `http.Get` with a configurable timeout
// and truncates the body to 8 KiB. AutoGPT upstream's analogous
// component (`forge/components/web_browser/`) does much more (HTML
// rendering, link extraction, BeautifulSoup parsing); we ship the
// minimum that's useful — fetch + truncate — and leave richer parsing
// as exercise.
//
// Why truncate at 8 KiB? Because raw HTML pages are often 100s of KiB,
// and feeding all of that into a context window wastes tokens (and
// money). 8 KiB is roughly 2 K tokens — enough to read structure or
// short articles, small enough to keep cost predictable. The truncation
// marker "... (truncated)" is appended so the model knows the body was
// cut.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// webFetchMaxBytes is the per-fetch body cap. Pulled out as a const so
// tests can reference the same value without re-encoding it.
const webFetchMaxBytes = 8192

// webFetchTruncateMarker is appended when the body exceeds the cap.
const webFetchTruncateMarker = "\n... (truncated)"

// WebFetchComponent bundles a single web_fetch tool. The httpTimeout
// is captured at construction so different agents can use different
// timeout policies without rewriting the tool.
type WebFetchComponent struct {
	httpTimeout time.Duration
}

// NewWebFetchComponent builds a component with the given timeout.
// A zero timeout is interpreted as 30 seconds (the default Go HTTP
// client has no timeout, which is dangerous for an LLM-driven loop).
func NewWebFetchComponent(timeout time.Duration) *WebFetchComponent {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &WebFetchComponent{httpTimeout: timeout}
}

// Commands implements CommandProvider. Returns one tool, configured
// with the component's timeout.
func (w *WebFetchComponent) Commands() []Tool {
	return []Tool{newWebFetchTool(w.httpTimeout)}
}

// webFetchTool is the actual Tool. Lower-cased because callers should
// build it via the component, not directly — the Component is the
// public API.
type webFetchTool struct {
	client *http.Client
}

// newWebFetchTool builds a tool with a fresh http.Client. We construct
// a new client per tool (rather than reusing http.DefaultClient) so the
// per-component timeout actually takes effect; DefaultClient has no
// timeout, which is exactly what we're trying to avoid.
func newWebFetchTool(timeout time.Duration) *webFetchTool {
	return &webFetchTool{
		client: &http.Client{Timeout: timeout},
	}
}

// Schema describes the tool to the LLM. Single required field `url`.
func (w *webFetchTool) Schema() ToolSchema {
	return ToolSchema{
		Name:        "web_fetch",
		Description: fmt.Sprintf("HTTP GET a URL and return the response body as a string. Body is truncated to %d bytes; the marker \"... (truncated)\" is appended when truncation happened. Use for short reads of public APIs / documentation; not a full web browser.", webFetchMaxBytes),
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type":        "string",
					"description": "Absolute http(s) URL to fetch. Relative paths and non-http schemes are rejected.",
				},
			},
			"required": []string{"url"},
		},
	}
}

// Execute does the GET. Errors fall into a few buckets:
//
//   - missing/invalid `url` field → tool error (model can retry with a
//     different shape).
//   - non-2xx response → tool error including the status (model sees
//     what HTTP said).
//   - network error → wrapped error (model sees the underlying cause).
//   - 2xx → body returned as a string, truncated to webFetchMaxBytes
//     with the truncation marker appended.
//
// We pass `ctx` to NewRequestWithContext so cancellation works, but
// the http.Client.Timeout is the actual enforcement. Both belt and
// suspenders.
func (w *webFetchTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	rawURL, err := requireString(input, "url")
	if err != nil {
		return "", fmt.Errorf("web_fetch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("web_fetch: build request: %w", err)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_fetch: %w", err)
	}
	defer resp.Body.Close()

	// Read up to webFetchMaxBytes+1; the +1 byte tells us whether
	// truncation happened without a separate Content-Length check
	// (which servers may lie about or omit for chunked responses).
	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("web_fetch: read body: %w", err)
	}

	truncated := false
	if len(body) > webFetchMaxBytes {
		body = body[:webFetchMaxBytes]
		truncated = true
	}

	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("web_fetch: HTTP %d: %s", resp.StatusCode, string(body))
	}

	out := string(body)
	if truncated {
		out += webFetchTruncateMarker
	}
	return out, nil
}

// Compile-time assertions.
var (
	_ Component       = (*WebFetchComponent)(nil)
	_ CommandProvider = (*WebFetchComponent)(nil)
	_ Tool            = (*webFetchTool)(nil)
)
