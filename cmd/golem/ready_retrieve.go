package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
type readyRetrieve struct {
	mu       sync.RWMutex
	state    retrieveReadyState
	reader   *retrievalReader
	message  string
	closed   bool // close() ran; a later install must retire the incoming reader
	retiring sync.WaitGroup
	closeErr error
}

// retrievalReader owns one immutable generation's model-facing tool and all
// resources that must outlive its admitted calls. readyRetrieve serializes
// WaitGroup admission against replacement; closeAfterDrain is therefore never
// concurrent with a new Add for the retired reader.
type retrievalReader struct {
	tool      agent.Tool
	inflight  sync.WaitGroup
	closeOnce sync.Once
	closeFn   func() error
	closeErr  error
}

func newRetrievalReader(tool agent.Tool, closeFn func() error) *retrievalReader {
	return &retrievalReader{tool: tool, closeFn: closeFn}
}

func newOwnedRetrievalReader(tool agent.Tool, store interface{ Close() error }) *retrievalReader {
	reader := &retrievalReader{tool: tool}
	reader.closeFn = func() error {
		var closeErr error
		if store != nil {
			if err := store.Close(); err != nil {
				closeErr = errors.Join(closeErr, fmt.Errorf("golem: close retrieval generation store: %w", err))
			}
		}
		return closeErr
	}
	return reader
}

func (r *retrievalReader) closeAfterDrain() error {
	if r == nil {
		return nil
	}
	r.inflight.Wait()
	r.closeOnce.Do(func() {
		if r.closeFn != nil {
			r.closeErr = r.closeFn()
		}
	})
	return r.closeErr
}

// newReadyRetrieve returns a wrapper in the warming state; warmingMessage is
// served to the model until a terminal transition.
func newReadyRetrieve(warmingMessage string) *readyRetrieve {
	return &readyRetrieve{message: warmingMessage}
}

func (r *readyRetrieve) Spec() agent.ToolSpec { return agenttools.Retrieve{}.Spec() }

func (r *readyRetrieve) Effect() agent.Effect { return agenttools.Retrieve{}.Effect() }

// Origin mirrors Spec and Effect (#514 D6): the wrapper serves the library's
// retrieve tool in every state, so it declares what that tool declares.
func (r *readyRetrieve) Origin() agent.Origin { return agenttools.Retrieve{}.Origin() }

var _ agent.OriginTool = (*readyRetrieve)(nil)

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
			r.recordCloseError(reader.closeAfterDrain())
		}
		return false
	}
	r.mu.Lock()
	if r.closed || r.state == retrieveFailed {
		r.mu.Unlock()
		r.recordCloseError(reader.closeAfterDrain())
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
			r.recordCloseError(old.closeAfterDrain())
		}()
	}
	return true
}

// markFailed records the terminal failure message served to the model. First
// terminal transition wins, mirroring install.
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

// recordCloseError retains asynchronous retirement failures for close.
func (r *readyRetrieve) recordCloseError(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	r.closeErr = errors.Join(r.closeErr, err)
	r.mu.Unlock()
}

func (r *readyRetrieve) close() error {
	r.mu.Lock()
	reader := r.reader
	r.reader = nil
	r.closed = true
	r.mu.Unlock()
	r.recordCloseError(reader.closeAfterDrain())
	r.retiring.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeErr
}
