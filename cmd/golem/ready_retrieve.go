package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// retrieveReadyState tracks startup and replacement. Only pre-ready failure
// is terminal; a detached ready wrapper can accept a verified replacement.
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

const unavailableRetrieveMessage = "retrieve: the workspace index is unavailable; " +
	"use read_file, search, glob, and list instead; reopen retrieval after indexing completes."

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
	valid     func(context.Context) bool // immutable managed generation admission check; nil for explicit DBs
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

// Origin mirrors Spec and Effect (#514 D6): the only delegate this wrapper
// ever installs is the library retrieve tool, and its warming and failure
// messages are Golem-authored text, so it declares what that tool declares.
// A per-invocation ToolResult.Origin from the delegate passes through Invoke
// untouched and may only lower trust (#439).
func (r *readyRetrieve) Origin() agent.Origin { return agenttools.Retrieve{}.Origin() }

var _ agent.OriginTool = (*readyRetrieve)(nil)

// Invoke serves the state message while warming/failed and delegates once
// ready. Warming/failed results are non-error so the model falls back to the
// file tools instead of abandoning tool use.
func (r *readyRetrieve) Invoke(ctx context.Context, args json.RawMessage) (agent.ToolResult, error) {
	r.mu.RLock()
	state, reader, message := r.state, r.reader, r.message
	r.mu.RUnlock()
	if state != retrieveReady || reader == nil {
		return agent.ToolResult{Content: message}, nil
	}
	// Pointer I/O never holds the wrapper mutex. A canceled caller must not
	// invalidate a healthy generation; retrieval itself keeps the caller's ctx.
	if reader.valid != nil && !reader.valid(context.WithoutCancel(ctx)) {
		r.detach(reader)
		return agent.ToolResult{Content: unavailableRetrieveMessage}, nil
	}
	r.mu.RLock()
	if r.closed || r.reader != reader {
		r.mu.RUnlock()
		return agent.ToolResult{Content: unavailableRetrieveMessage}, nil
	}
	reader.inflight.Add(1)
	r.mu.RUnlock()
	defer reader.inflight.Done()
	return reader.tool.Invoke(ctx, args)
}

// bindGeneration captures immutable identity at each managed construction site.
// Admission validates only the active pointer, never the generation's SQLite DB.
func (r *retrievalReader) bindGeneration(dbPath, workspaceID string, gen indexGeneration) {
	r.valid = func(ctx context.Context) bool {
		pointer, err := readActivePointer(ctx, dbPath)
		if gen.legacy {
			return errors.Is(err, os.ErrNotExist)
		}
		return err == nil && validatePointer(pointer, workspaceID) == nil && !pointer.Retired && pointer.Generation == gen.id
	}
}

// detach retires only the checked reader. An older validation cannot remove a
// newer installation, and pointer invalidation leaves the wrapper replaceable.
func (r *readyRetrieve) detach(reader *retrievalReader) {
	r.mu.Lock()
	if r.closed || r.state == retrieveFailed || r.reader != reader {
		r.mu.Unlock()
		return
	}
	r.reader = nil
	r.state = retrieveReady
	r.message = unavailableRetrieveMessage
	if reader != nil {
		r.retiring.Add(1)
	}
	r.mu.Unlock()
	r.drainRetired(reader)
}

func (r *readyRetrieve) drainRetired(reader *retrievalReader) {
	if reader == nil {
		return
	}
	go func() {
		defer r.retiring.Done()
		r.recordCloseError(reader.closeAfterDrain())
	}()
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
	r.drainRetired(old)
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
