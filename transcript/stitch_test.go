package transcript

import (
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/conversation"
)

// msgs builds a message slice with alternating user/assistant roles. The roles
// are irrelevant to decideStitch (it compares canonical content) but keep the
// fixtures realistic.
func msgs(contents ...string) []conversation.Message {
	out := make([]conversation.Message, len(contents))
	for i, c := range contents {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out[i] = conversation.Message{Role: role, Content: c}
	}
	return out
}

func TestDecideStitch_Created(t *testing.T) {
	dec := decideStitch("k", msgs("a", "b"), nil, time.UnixMilli(1000), defaultLeaseWindow, defaultShortThreshold)
	if dec.status != statusCreated || dec.targetID != "k" {
		t.Errorf("got %+v, want {k created}", dec)
	}
}

func TestDecideStitch_Idempotent(t *testing.T) {
	c := candidate{id: "k", messages: msgs("a", "b"), messageCount: 2, updatedAt: 900}
	dec := decideStitch("k", msgs("a", "b"), []candidate{c}, time.UnixMilli(1000), defaultLeaseWindow, defaultShortThreshold)
	if dec.status != statusIdempotent || dec.targetID != "k" {
		t.Errorf("got %+v, want {k idempotent}", dec)
	}
	if dec.preserveRendered {
		t.Errorf("preserveRendered = true for exact idempotent retry; want false so rendered can refresh")
	}
}

func TestDecideStitch_Extended(t *testing.T) {
	c := candidate{id: "k", messages: msgs("a", "b"), messageCount: 2, updatedAt: 900}
	dec := decideStitch("k", msgs("a", "b", "c", "d"), []candidate{c}, time.UnixMilli(1000), defaultLeaseWindow, defaultShortThreshold)
	if dec.status != statusExtended || dec.targetID != "k" {
		t.Errorf("got %+v, want {k extended}", dec)
	}
}

func TestDecideStitch_ShorterIncomingNoShrink(t *testing.T) {
	c := candidate{id: "k", messages: msgs("a", "b", "c", "d"), messageCount: 4, updatedAt: 900}
	dec := decideStitch("k", msgs("a", "b"), []candidate{c}, time.UnixMilli(1000), defaultLeaseWindow, defaultShortThreshold)
	if dec.status != statusIdempotent || dec.targetID != "k" {
		t.Errorf("got %+v, want {k idempotent} (no shrink)", dec)
	}
	if !dec.preserveRendered {
		t.Errorf("preserveRendered = false for shorter incoming retry; want true to avoid shrinking rendered_messages")
	}
}

func TestDecideStitch_DivergentForks(t *testing.T) {
	c := candidate{id: "k", messages: msgs("a", "b"), messageCount: 2, updatedAt: 900}
	incoming := msgs("x", "y")
	dec := decideStitch("k", incoming, []candidate{c}, time.UnixMilli(1000), defaultLeaseWindow, defaultShortThreshold)
	if dec.status != statusForked {
		t.Fatalf("got %+v, want forked", dec)
	}
	if dec.targetID != forkID("k", incoming) {
		t.Errorf("fork target = %q, want %q", dec.targetID, forkID("k", incoming))
	}
}

func TestDecideStitch_ExtendsSiblingNotBase(t *testing.T) {
	// Once a session has forked, its later turns still derive the base key, so
	// the decision must extend the matching sibling rather than mint a new fork.
	base := candidate{id: "k", messages: msgs("a", "b"), messageCount: 2, updatedAt: 950}
	sibling := candidate{id: "k:abc123def456", messages: msgs("p", "q"), messageCount: 2, updatedAt: 950}
	incoming := msgs("p", "q", "r", "s")
	dec := decideStitch("k", incoming, []candidate{base, sibling}, time.UnixMilli(1000), defaultLeaseWindow, defaultShortThreshold)
	if dec.status != statusExtended || dec.targetID != "k:abc123def456" {
		t.Errorf("got %+v, want {k:abc123def456 extended}", dec)
	}
}

func TestForkID_ContentAddressed(t *testing.T) {
	incoming := msgs("a", "b")
	sameHistory := msgs("a", "b")
	if forkID("k", incoming) != forkID("k", sameHistory) {
		t.Fatal("forkID not deterministic for identical history")
	}
	if forkID("k", incoming) == forkID("k", msgs("a", "c")) {
		t.Error("forkID should differ for different histories")
	}
}

func TestDecideStitch_LeaseGuard(t *testing.T) {
	const leaseWindow = 12 * time.Hour
	const short = 2
	base := time.UnixMilli(0)
	now := base.Add(13 * time.Hour) // 1h past the lease window vs updatedAt=0

	incoming := msgs("a", "b", "c", "d")

	t.Run("stale and short forks", func(t *testing.T) {
		stub := candidate{id: "k", messages: msgs("a", "b"), messageCount: 2, updatedAt: 0}
		dec := decideStitch("k", incoming, []candidate{stub}, now, leaseWindow, short)
		if dec.status != statusForked {
			t.Errorf("stale+short prefix should fork, got %+v", dec)
		}
	})

	t.Run("fresh extends", func(t *testing.T) {
		fresh := candidate{id: "k", messages: msgs("a", "b"), messageCount: 2, updatedAt: now.Add(-time.Minute).UnixMilli()}
		dec := decideStitch("k", incoming, []candidate{fresh}, now, leaseWindow, short)
		if dec.status != statusExtended {
			t.Errorf("fresh prefix should extend, got %+v", dec)
		}
	})

	t.Run("long extends even when stale", func(t *testing.T) {
		long := candidate{id: "k", messages: msgs("a", "b", "c"), messageCount: 3, updatedAt: 0}
		incomingLong := msgs("a", "b", "c", "d", "e")
		dec := decideStitch("k", incomingLong, []candidate{long}, now, leaseWindow, short)
		if dec.status != statusExtended {
			t.Errorf("stale but long prefix should extend, got %+v", dec)
		}
	})
}
