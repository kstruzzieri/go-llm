package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/conversation"
)

// fillSession appends n user/assistant exchanges of the given content.
func fillSession(t *testing.T, s *session, n int, body string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.record(context.Background(), body, body); err != nil {
			t.Fatal(err)
		}
	}
}

func newCompressRepl(s *session, sum conversation.Summarizer, maxHistory int, enabled bool) *replSession {
	return &replSession{
		session: s,
		compress: compressPolicy{
			summarize:          sum,
			estimate:           conversation.CharRatioEstimator(4.0),
			maxHistoryTokens:   maxHistory,
			minRecentExchanges: 1,
			enabled:            enabled,
		},
	}
}

func TestMaybeCompress_FiresOverThreshold(t *testing.T) {
	ctx := context.Background()
	s, _ := openTempSession(t, "workspace:fire")
	fillSession(t, s, 8, "this is a reasonably long line of conversation text")

	var called bool
	sum := func(_ context.Context, prior string, msgs []conversation.Message) (string, error) {
		called = true
		return "ROLLED-UP", nil
	}
	sess := newCompressRepl(s, sum, 20, true) // tiny budget => must fire

	if err := sess.maybeCompress(ctx); err != nil {
		t.Fatalf("maybeCompress: %v", err)
	}
	if !called {
		t.Fatal("summarizer was not called")
	}
	if s.historySummary() != "ROLLED-UP" {
		t.Fatalf("summary not applied: %q", s.historySummary())
	}
	if len(s.msgs) >= 16 {
		t.Fatalf("history not reduced: %d msgs", len(s.msgs))
	}
}

func TestMaybeCompress_NoOpUnderThreshold(t *testing.T) {
	ctx := context.Background()
	s, _ := openTempSession(t, "workspace:under")
	fillSession(t, s, 2, "short")

	called := false
	sum := func(context.Context, string, []conversation.Message) (string, error) {
		called = true
		return "X", nil
	}
	sess := newCompressRepl(s, sum, 100000, true) // huge budget => never fire

	if err := sess.maybeCompress(ctx); err != nil {
		t.Fatalf("maybeCompress: %v", err)
	}
	if called {
		t.Fatal("summarizer called under threshold")
	}
}

func TestMaybeCompress_DisabledIsNoOp(t *testing.T) {
	ctx := context.Background()
	s, _ := openTempSession(t, "workspace:off")
	fillSession(t, s, 8, "this is a reasonably long line of conversation text")

	called := false
	sum := func(context.Context, string, []conversation.Message) (string, error) {
		called = true
		return "X", nil
	}
	sess := newCompressRepl(s, sum, 20, false) // disabled

	if err := sess.maybeCompress(ctx); err != nil {
		t.Fatalf("maybeCompress: %v", err)
	}
	if called {
		t.Fatal("summarizer called while disabled")
	}
}

func TestMaybeCompress_SummarizerErrorLeavesSessionIntact(t *testing.T) {
	ctx := context.Background()
	s, _ := openTempSession(t, "workspace:err")
	fillSession(t, s, 8, "this is a reasonably long line of conversation text")
	before := len(s.msgs)

	sum := func(context.Context, string, []conversation.Message) (string, error) {
		return "", errors.New("model down")
	}
	sess := newCompressRepl(s, sum, 20, true)

	err := sess.maybeCompress(ctx)
	if err == nil || !strings.Contains(err.Error(), "model down") {
		t.Fatalf("want surfaced summarizer error, got %v", err)
	}
	if len(s.msgs) != before || s.summary != nil {
		t.Fatalf("session mutated on failure: msgs=%d summary=%v", len(s.msgs), s.summary)
	}
}
