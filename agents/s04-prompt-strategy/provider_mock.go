package main

import (
	"context"
	"fmt"
)

// MockProvider replays a fixed slice of responses, one per CreateMessage
// call, and records every request for later assertion. Designed to be
// reused verbatim by every later session's tests — keep it minimal.
type MockProvider struct {
	Responses []*CreateMessageResponse
	Requests  []CreateMessageRequest
	idx       int
}

func NewMockProvider(responses ...*CreateMessageResponse) *MockProvider {
	return &MockProvider{Responses: responses}
}

func (m *MockProvider) CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error) {
	m.Requests = append(m.Requests, req)
	if m.idx >= len(m.Responses) {
		return nil, fmt.Errorf("mock provider exhausted at call %d (only %d responses queued)", m.idx, len(m.Responses))
	}
	r := m.Responses[m.idx]
	m.idx++
	return r, nil
}

// Compile-time check.
var _ Provider = (*MockProvider)(nil)
