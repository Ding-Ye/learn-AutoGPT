package main

import (
	"context"
	"strings"
	"testing"
)

func TestMockProvider_PlaysInOrder(t *testing.T) {
	r1 := &CreateMessageResponse{ID: "r1", StopReason: "end_turn"}
	r2 := &CreateMessageResponse{ID: "r2", StopReason: "end_turn"}
	m := NewMockProvider(r1, r2)

	got1, err := m.CreateMessage(context.Background(), CreateMessageRequest{})
	if err != nil {
		t.Fatalf("call 1 err: %v", err)
	}
	if got1.ID != "r1" {
		t.Errorf("call 1: got %q, want r1", got1.ID)
	}

	got2, err := m.CreateMessage(context.Background(), CreateMessageRequest{})
	if err != nil {
		t.Fatalf("call 2 err: %v", err)
	}
	if got2.ID != "r2" {
		t.Errorf("call 2: got %q, want r2", got2.ID)
	}
}

func TestMockProvider_ErrorsOnExhaustion(t *testing.T) {
	m := NewMockProvider(&CreateMessageResponse{StopReason: "end_turn"})
	if _, err := m.CreateMessage(context.Background(), CreateMessageRequest{}); err != nil {
		t.Fatalf("first call should succeed: %v", err)
	}
	_, err := m.CreateMessage(context.Background(), CreateMessageRequest{})
	if err == nil {
		t.Fatal("expected exhaustion error on second call")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("error %q should mention 'exhausted'", err.Error())
	}
}

func TestMockProvider_RecordsRequests(t *testing.T) {
	m := NewMockProvider(
		&CreateMessageResponse{StopReason: "end_turn"},
		&CreateMessageResponse{StopReason: "end_turn"},
	)
	req1 := CreateMessageRequest{Model: "first", Messages: []Message{{Role: "user"}}}
	req2 := CreateMessageRequest{Model: "second"}

	if _, err := m.CreateMessage(context.Background(), req1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateMessage(context.Background(), req2); err != nil {
		t.Fatal(err)
	}
	if len(m.Requests) != 2 {
		t.Fatalf("got %d recorded requests, want 2", len(m.Requests))
	}
	if m.Requests[0].Model != "first" || m.Requests[1].Model != "second" {
		t.Errorf("recorded request order is wrong: %+v", m.Requests)
	}
}
