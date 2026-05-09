package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestWebFetchComponent_Schema — the schema sanity test: name,
// description, required field. This is what the model sees when
// deciding whether to call the tool.
func TestWebFetchComponent_Schema(t *testing.T) {
	c := NewWebFetchComponent(5 * time.Second)
	cmds := c.Commands()
	if len(cmds) != 1 {
		t.Fatalf("Commands len = %d, want 1", len(cmds))
	}
	s := cmds[0].Schema()
	if s.Name != "web_fetch" {
		t.Errorf("Schema().Name = %q, want \"web_fetch\"", s.Name)
	}
	required, _ := s.InputSchema["required"].([]string)
	if len(required) != 1 || required[0] != "url" {
		t.Errorf("required = %v, want [\"url\"]", required)
	}
	props, _ := s.InputSchema["properties"].(map[string]interface{})
	if _, ok := props["url"]; !ok {
		t.Errorf("properties does not contain 'url': %v", props)
	}
	// Description must mention truncation behavior so the model knows
	// what it's getting.
	if !strings.Contains(s.Description, "truncated") {
		t.Errorf("Description %q must mention truncation", s.Description)
	}
}

// TestWebFetchComponent_ExecuteFetchesViaHttptest — the integration
// test: spin up an httptest.Server, point the tool at it, assert the
// returned body matches what the server wrote. This is the canonical
// pattern from the catalog (no real network calls in tests).
func TestWebFetchComponent_ExecuteFetchesViaHttptest(t *testing.T) {
	t.Run("happy path: 2xx returns body verbatim", func(t *testing.T) {
		const want = "hello from httptest"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(want))
		}))
		defer srv.Close()

		c := NewWebFetchComponent(5 * time.Second)
		tool := c.Commands()[0]
		out, err := tool.Execute(context.Background(), map[string]interface{}{"url": srv.URL})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if out != want {
			t.Errorf("body = %q, want %q", out, want)
		}
	})

	t.Run("truncation: large body cut at cap with marker", func(t *testing.T) {
		// Server emits 20 KiB of A's — should truncate to 8192 bytes
		// plus the truncation marker.
		bigBody := strings.Repeat("A", 20*1024)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(bigBody))
		}))
		defer srv.Close()

		c := NewWebFetchComponent(5 * time.Second)
		tool := c.Commands()[0]
		out, err := tool.Execute(context.Background(), map[string]interface{}{"url": srv.URL})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.HasSuffix(out, webFetchTruncateMarker) {
			t.Errorf("truncated body must end with %q; got tail %q",
				webFetchTruncateMarker, out[max(0, len(out)-32):])
		}
		// Body content (without the marker) must be exactly the cap.
		bodyOnly := strings.TrimSuffix(out, webFetchTruncateMarker)
		if len(bodyOnly) != webFetchMaxBytes {
			t.Errorf("truncated body len = %d, want %d", len(bodyOnly), webFetchMaxBytes)
		}
	})

	t.Run("non-2xx surfaces as error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("nope"))
		}))
		defer srv.Close()

		c := NewWebFetchComponent(5 * time.Second)
		tool := c.Commands()[0]
		_, err := tool.Execute(context.Background(), map[string]interface{}{"url": srv.URL})
		if err == nil {
			t.Fatal("expected error on 404, got nil")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("error %q must mention HTTP status 404", err.Error())
		}
	})
}

// max is a tiny helper to keep slicing safe in the truncation test.
// Go 1.22 lacks builtin generic max for ints in older modes; this is
// trivial enough to inline.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
