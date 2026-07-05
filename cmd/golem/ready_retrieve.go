package main

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// retrieveReadyState is the readyRetrieve lifecycle: warming until the
// background auto-index job flips it to exactly one terminal state.
type retrieveReadyState int

const (
	retrieveWarming retrieveReadyState = iota
	retrieveReady
	retrieveFailed
)

// warmingRetrieveMessage steers the model to the file tools while the
// background job runs. Non-error by design: an index that is not ready yet is
// a normal observation, not a malformed call.
const warmingRetrieveMessage = "retrieve: the workspace index is warming in the background; " +
	"use read_file, search, glob, and list for now and retry retrieve later in this session."

// readyRetrieve is the late-binding retrieve tool registered in default auto
// mode. Spec/Effect mirror agenttools.Retrieve (both are static on the empty
// value) so the model-facing schema is identical before and after the swap.
// Invoke may run from parallel read-only dispatch goroutines (#235) while the
// background job transitions state, so all state is RWMutex-guarded; Invoke
// copies the delegate under lock and never holds the lock during retrieval.
// The wrapper owns any behavioralWeighterHandle installed with the delegate
// and releases it in close().
type readyRetrieve struct {
	mu       sync.RWMutex
	state    retrieveReadyState
	tool     agent.Tool
	feedback *behavioralWeighterHandle
	message  string
	closed   bool // close() ran; a later markReady must not strand a handle
}

// newReadyRetrieve returns a wrapper in the warming state; warmingMessage is
// served to the model until a terminal transition.
func newReadyRetrieve(warmingMessage string) *readyRetrieve {
	return &readyRetrieve{message: warmingMessage}
}

func (r *readyRetrieve) Spec() agent.ToolSpec { return agenttools.Retrieve{}.Spec() }

func (r *readyRetrieve) Effect() agent.Effect { return agenttools.Retrieve{}.Effect() }

// Invoke serves the state message while warming/failed and delegates once
// ready. Warming/failed results are non-error so the model falls back to the
// file tools instead of abandoning tool use.
func (r *readyRetrieve) Invoke(ctx context.Context, args json.RawMessage) (agent.ToolResult, error) {
	r.mu.RLock()
	state, tool, message := r.state, r.tool, r.message
	r.mu.RUnlock()
	if state == retrieveReady {
		return tool.Invoke(ctx, args)
	}
	return agent.ToolResult{Content: message}, nil
}

// markReady installs the opened retriever, records the ready message (kept
// for diagnostics; Invoke delegates instead of serving it), and takes
// ownership of the feedback handle that buildGatedRetriever returned
// alongside the retriever (nil when behavioral ranking is off). First
// terminal transition wins: a late markReady after markFailed is ignored so
// racing outcomes cannot flap the tool.
func (r *readyRetrieve) markReady(tool agent.Tool, message string, feedback *behavioralWeighterHandle) {
	r.mu.Lock()
	if r.state == retrieveWarming {
		r.state = retrieveReady
		r.tool = tool
		r.message = message
		if !r.closed {
			r.feedback = feedback
			feedback = nil // ownership transferred to close()
		}
	}
	r.mu.Unlock()
	// Not installed (lost the transition race, or close() already ran during
	// shutdown): nothing will ever release the handle, so release it here.
	if feedback != nil && feedback.db != nil {
		_ = feedback.db.Close()
	}
}

// markFailed records the terminal failure message served to the model. First
// terminal transition wins, mirroring markReady.
func (r *readyRetrieve) markFailed(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != retrieveWarming {
		return
	}
	r.state = retrieveFailed
	r.message = message
}

// close releases the retained feedback DB handle. Nil-safe and idempotent so
// main can defer it unconditionally in auto mode.
func (r *readyRetrieve) close() {
	r.mu.Lock()
	f := r.feedback
	r.feedback = nil
	r.closed = true
	r.mu.Unlock()
	if f != nil && f.db != nil {
		_ = f.db.Close()
	}
}
