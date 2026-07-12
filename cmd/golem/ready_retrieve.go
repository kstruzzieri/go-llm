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
	reader   *retrievalReader
	message  string
	closed   bool // close() ran; a later install must retire the incoming reader
	retiring sync.WaitGroup
}

// retrievalReader owns one immutable generation's model-facing tool and all
// resources that must outlive its admitted calls. readyRetrieve serializes
// WaitGroup admission against replacement; closeAfterDrain is therefore never
// concurrent with a new Add for the retired reader.
type retrievalReader struct {
	tool      agent.Tool
	store     interface{ Close() error }
	feedback  *behavioralWeighterHandle
	inflight  sync.WaitGroup
	closeOnce sync.Once
	closeFn   func()
}

func newRetrievalReader(tool agent.Tool, closeFn func()) *retrievalReader {
	return &retrievalReader{tool: tool, closeFn: closeFn}
}

func newOwnedRetrievalReader(tool agent.Tool, store interface{ Close() error }, feedback *behavioralWeighterHandle) *retrievalReader {
	reader := &retrievalReader{tool: tool, store: store, feedback: feedback}
	reader.closeFn = func() {
		if feedback != nil && feedback.db != nil {
			_ = feedback.db.Close()
		}
		if store != nil {
			_ = store.Close()
		}
	}
	return reader
}

func (r *retrievalReader) closeAfterDrain() {
	if r == nil {
		return
	}
	r.inflight.Wait()
	r.closeOnce.Do(func() {
		if r.closeFn != nil {
			r.closeFn()
		}
	})
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
	state, reader, message := r.state, r.reader, r.message
	if state == retrieveReady && reader != nil {
		reader.inflight.Add(1)
	}
	r.mu.RUnlock()
	if state == retrieveReady && reader != nil {
		defer reader.inflight.Done()
		return reader.tool.Invoke(ctx, args)
	}
	return agent.ToolResult{Content: message}, nil
}

// install publishes reader for new calls and retires the previous generation
// after its already-admitted calls finish. It returns false when shutdown or a
// terminal pre-ready failure rejects the reader; rejected readers are closed.
func (r *readyRetrieve) install(reader *retrievalReader, message string) bool {
	if reader == nil || reader.tool == nil {
		if reader != nil {
			reader.closeAfterDrain()
		}
		return false
	}
	r.mu.Lock()
	if r.closed || r.state == retrieveFailed {
		r.mu.Unlock()
		reader.closeAfterDrain()
		return false
	}
	old := r.reader
	r.reader = reader
	r.state = retrieveReady
	r.message = message
	if old != nil {
		r.retiring.Add(1)
	}
	r.mu.Unlock()
	if old != nil {
		go func() {
			defer r.retiring.Done()
			old.closeAfterDrain()
		}()
	}
	return true
}

// markReady installs the opened retriever, records the ready message (kept
// for diagnostics; Invoke delegates instead of serving it), and takes
// ownership of the feedback handle that buildGatedRetriever returned
// alongside the retriever (nil when behavioral ranking is off). First
// terminal transition wins: a late markReady after markFailed is ignored so
// racing outcomes cannot flap the tool.
func (r *readyRetrieve) markReady(tool agent.Tool, message string, feedback *behavioralWeighterHandle) {
	r.install(newRetrievalReader(tool, func() {
		if feedback != nil && feedback.db != nil {
			_ = feedback.db.Close()
		}
	}), message)
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

func (r *readyRetrieve) hasReader() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reader != nil
}

// close releases the retained feedback DB handle. Nil-safe and idempotent so
// main can defer it unconditionally in auto mode.
func (r *readyRetrieve) close() {
	r.mu.Lock()
	reader := r.reader
	r.reader = nil
	r.closed = true
	r.mu.Unlock()
	reader.closeAfterDrain()
	r.retiring.Wait()
}
