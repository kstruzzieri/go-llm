package golem

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/internal/datadir"
	"github.com/kstruzzieri/go-llm/internal/pathguard"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/provider"
)

const (
	memorySearchRedactedMarker    = "memory_search result omitted from session history"
	agentMemoryRedactedMarker     = "agent memory tool result omitted from session history"
	agentMemoryArgsRedactedMarker = `{"redacted":"agent memory arguments omitted from session history"}`
)

type threadStore struct {
	db    *sql.DB
	path  string
	store SessionStore
}

type threadState struct {
	conversation conversation.Conversation
}

func canonicalRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("golem: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("golem: resolve root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("golem: canonicalize root %q: %w", abs, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("golem: stat root %q: %w", canonical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("golem: root %q is not a directory", canonical)
	}
	return canonical, nil
}

func (r *Runtime) loadThread(ctx context.Context, id string) (*threadState, error) {
	store, err := r.threadStore(ctx)
	if err != nil {
		return nil, err
	}
	conv, err := store.store.Load(ctx, id)
	if err != nil {
		if errors.Is(err, conversation.ErrNotFound) {
			return &threadState{conversation: conversation.Conversation{ID: id}}, nil
		}
		return nil, fmt.Errorf("golem: load thread %q: %w", id, err)
	}
	if conv == nil || conv.ID != id {
		return nil, fmt.Errorf("golem: load thread %q: session store returned invalid conversation", id)
	}
	return &threadState{conversation: *conv}, nil
}

func (s *threadState) history() []provider.ChatMessage {
	history := make([]provider.ChatMessage, 0, len(s.conversation.Messages))
	for _, message := range s.conversation.Messages {
		if (message.Role == "user" || message.Role == "assistant") && message.Content != "" {
			history = append(history, provider.ChatMessage{Role: message.Role, Content: message.Content})
		}
	}
	return history
}

func (s *threadState) summary() string {
	if s.conversation.DurableSummary == nil {
		return ""
	}
	return s.conversation.DurableSummary.Content
}

// CompactThread summarizes stored history to the existing retention floor and
// atomically saves an actual change. An active turn or compaction on threadID
// returns ErrRunConflict; disabled compression returns ErrCompressionUnavailable.
// The caller context and Close cancel work. Save returning nil determines
// success, even if cancellation races afterward. On an error after loading,
// the report retains the before estimate with Changed false; before loading it
// is zero. An unchanged result may still incur a model request to consolidate
// an existing summary. With compression enabled, a well-formed missing thread ID
// returns a zero, unchanged report and nil error without inserting a conversation.
func (r *Runtime) CompactThread(ctx context.Context, threadID string) (CompactionReport, error) {
	var report CompactionReport
	if threadID == "" || len(threadID) > maxCorrelationIDBytes {
		return report, fmt.Errorf("%w: thread ID must contain 1 to %d bytes", ErrInvalidRequest, maxCorrelationIDBytes)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return report, ErrClosed
	}
	if !r.compress || r.summarizer == nil {
		r.mu.Unlock()
		return report, ErrCompressionUnavailable
	}
	if _, busy := r.activeThreads[threadID]; busy {
		r.mu.Unlock()
		return report, fmt.Errorf("%w: thread %q is active", ErrRunConflict, threadID)
	}
	r.activeThreads[threadID] = &activeRun{cancel: cancel}
	r.wg.Add(1)
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.activeThreads, threadID)
		r.mu.Unlock()
		r.wg.Done()
	}()
	if err := ctx.Err(); err != nil {
		return report, fmt.Errorf("golem: compact thread %q: %w", threadID, err)
	}
	state, err := r.loadThread(ctx, threadID)
	if err != nil {
		return report, err
	}
	report.TokensBefore = estimateStoredHistory(state.conversation)
	report.TokensAfter = report.TokensBefore
	if err := ctx.Err(); err != nil {
		return report, fmt.Errorf("golem: compact thread %q: %w", threadID, err)
	}
	candidate, changed, err := r.compressConversation(ctx, state.conversation, true)
	if err != nil {
		return report, fmt.Errorf("golem: compact thread %q: %w", threadID, err)
	}
	if err := ctx.Err(); err != nil {
		return report, fmt.Errorf("golem: compact thread %q: %w", threadID, err)
	}
	if !changed {
		return report, nil
	}
	store, err := r.threadStore(ctx)
	if err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, fmt.Errorf("golem: compact thread %q: %w", threadID, err)
	}
	if err := store.store.Save(ctx, candidate); err != nil {
		return report, fmt.Errorf("%w: compact thread %q: %w", ErrSessionPersistence, threadID, err)
	}
	r.secureThreadStore(store)
	report.TokensAfter = estimateStoredHistory(candidate)
	report.Changed = true
	return report, nil
}

func (r *Runtime) saveThread(ctx context.Context, active *activeRun, state *threadState, userMessage string, result agent.Result) error {
	messages, err := resultMessages(userMessage, result)
	if err != nil {
		return err
	}
	candidate := state.conversation
	candidate.Messages = append(append([]conversation.Message(nil), candidate.Messages...), messages...)
	candidate.Title = threadTitle(candidate.Messages)

	if r.commitTerminal(active, ctx, "run.finished") == "run.canceled" {
		return ctx.Err()
	}
	// Cancellation loses once finalization is claimed; otherwise a successful
	// save could still be reported as canceled and be duplicated on retry.
	persistCtx := context.WithoutCancel(ctx)
	store, err := r.threadStore(persistCtx)
	if err != nil {
		return err
	}
	if err := store.store.Save(persistCtx, candidate); err != nil {
		return fmt.Errorf("golem: save thread %q: %w", candidate.ID, err)
	}
	state.conversation = candidate
	// The turn is durable from here. Hardening and compression failures are
	// warnings, not run failures: reporting a failure for a committed turn
	// invites a consumer retry that would duplicate it.
	r.secureThreadStore(store)
	if !r.compress {
		return nil
	}
	compacted, changed, err := r.compressConversation(ctx, candidate, false)
	if err != nil {
		r.reportCompressionWarning(candidate.ID, err)
		return nil
	}
	if !changed {
		return nil
	}
	if err := store.store.Save(ctx, compacted); err != nil {
		r.reportCompressionWarning(candidate.ID, fmt.Errorf("save compressed conversation: %w", err))
		return nil
	}
	state.conversation = compacted
	r.secureThreadStore(store)
	return nil
}

func (r *Runtime) secureThreadStore(store *threadStore) {
	if store.db == nil {
		return
	}
	if err := memory.SecureDBFiles(store.path); err != nil {
		r.reportWarning(fmt.Errorf("golem: secure session database: %w", err))
	}
}

func (r *Runtime) reportWarning(err error) {
	if r.onWarning != nil {
		r.onWarning(err)
	}
}

func (r *Runtime) reportCompressionWarning(threadID string, err error) {
	r.reportWarning(fmt.Errorf("golem: compress thread %q: %w", threadID, err))
}

func (r *Runtime) compressConversation(ctx context.Context, current conversation.Conversation, force bool) (conversation.Conversation, bool, error) {
	estimate := conversation.CharRatioEstimator(4.0)
	maxHistoryTokens := r.budget.InputCeiling
	if maxHistoryTokens <= 0 {
		maxHistoryTokens = agent.DefaultInputCeiling
	}
	maxHistoryTokens /= 2
	if !force && estimateStoredHistory(current) <= maxHistoryTokens {
		return current, false, nil
	}
	// The model's output allowance excludes the rendered envelope and quoting.
	// Reserve the envelope up front and account for actual output below, since
	// escaping (or a custom summarizer) can exceed the anticipated content cost.
	reserve := max(agent.DefaultSummaryOutputReserve+estimate(agent.DurableSummaryPrompt("")),
		estimate(agent.DurableSummaryPrompt(strings.TrimSpace(currentSummary(current)))))
	input, summarize := current, r.summarizer
	var compacted conversation.Conversation
	for {
		maxRawTokens := max(0, maxHistoryTokens-reserve)
		if force {
			maxRawTokens = 0
		}
		var err error
		compacted, err = conversation.CompressMessages(ctx, input, maxRawTokens, 4, estimate, summarize)
		if err != nil {
			return conversation.Conversation{}, false, err
		}
		if force || maxRawTokens == 0 || estimateStoredHistory(compacted) <= maxHistoryTokens || len(compacted.Messages) >= len(input.Messages) {
			break
		}
		// Each retry must evict more history; the retention floor can still
		// exceed the budget. Fold additional messages into the new summary,
		// but do not pay for another rewrite when the floor keeps everything.
		input = compacted
		reserve = max(reserve, estimate(agent.DurableSummaryPrompt(currentSummary(compacted))))
		summarize = func(ctx context.Context, prior string, old []conversation.Message) (string, error) {
			if len(old) == 0 {
				return prior, nil
			}
			return r.summarizer(ctx, prior, old)
		}
	}
	changed := len(compacted.Messages) != len(current.Messages) ||
		currentSummary(compacted) != currentSummary(current) ||
		summaryMessageCount(compacted) != summaryMessageCount(current)
	return compacted, changed, nil
}

func estimateStoredHistory(current conversation.Conversation) int {
	estimate := conversation.CharRatioEstimator(4)
	tokens := conversation.EstimateMessagesTokens(current.Messages, estimate)
	if summary := strings.TrimSpace(currentSummary(current)); summary != "" {
		tokens += estimate(agent.DurableSummaryPrompt(summary))
	}
	return tokens
}

func currentSummary(current conversation.Conversation) string {
	if current.DurableSummary == nil {
		return ""
	}
	return current.DurableSummary.Content
}

func summaryMessageCount(current conversation.Conversation) int {
	if current.DurableSummary == nil {
		return 0
	}
	return current.DurableSummary.MessageCount
}

func resultMessages(userMessage string, result agent.Result) ([]conversation.Message, error) {
	if len(result.Messages) == 0 {
		return []conversation.Message{
			{Role: "user", Content: userMessage},
			{Role: "assistant", Content: result.Answer},
		}, nil
	}
	messages := make([]conversation.Message, len(result.Messages))
	for i, message := range result.Messages {
		messages[i] = conversation.Message{
			Role:       message.Role,
			Content:    message.Content,
			ToolName:   message.ToolName,
			ToolCallID: message.ToolCallID,
		}
		if i == 0 && message.Role == "user" {
			messages[i].Content = userMessage
		}
		if message.Role == "tool" {
			switch {
			case message.ToolName == agenttools.MemorySearchToolName:
				messages[i].Content = memorySearchRedactedMarker
			case isAgentMemoryTool(message.ToolName):
				messages[i].Content = agentMemoryRedactedMarker
			}
		}
		if len(message.ToolCalls) > 0 {
			raw, err := json.Marshal(redactAgentMemoryToolCalls(message.ToolCalls))
			if err != nil {
				return nil, fmt.Errorf("golem: marshal tool calls at message %d: %w", i, err)
			}
			messages[i].ToolCalls = raw
		}
	}
	return messages, nil
}

func isAgentMemoryTool(name string) bool {
	return name == agenttools.AgentMemorySearchToolName ||
		name == agenttools.AgentMemoryCreateToolName ||
		name == agenttools.AgentMemoryPromoteToolName
}

func redactAgentMemoryToolCalls(calls []provider.ToolCall) []provider.ToolCall {
	for i, call := range calls {
		if !isAgentMemoryTool(call.Function.Name) {
			continue
		}
		redacted := append([]provider.ToolCall(nil), calls...)
		for j := i; j < len(redacted); j++ {
			if isAgentMemoryTool(redacted[j].Function.Name) {
				redacted[j].Function.Arguments = json.RawMessage(agentMemoryArgsRedactedMarker)
			}
		}
		return redacted
	}
	return calls
}

func threadTitle(messages []conversation.Message) string {
	for _, message := range messages {
		if message.Role == "user" {
			runes := []rune(strings.TrimSpace(message.Content))
			if len(runes) > 60 {
				runes = runes[:60]
			}
			return string(runes)
		}
	}
	return ""
}

func (r *Runtime) threadStore(ctx context.Context) (*threadStore, error) {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	if r.sessions != nil {
		return r.sessions, nil
	}
	path, err := defaultSessionDBPath(r.root)
	if err != nil {
		return nil, err
	}
	db, err := memory.OpenHardenedDB(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("golem: open session database: %w", err)
	}
	store, err := conversation.NewStore(ctx, db)
	if err != nil {
		_ = db.Close()
		// Best-effort: migrations may have created loose sidecars before failing.
		_ = memory.SecureDBFiles(path)
		return nil, fmt.Errorf("golem: init session store: %w", err)
	}
	if err := memory.SecureDBFiles(path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("golem: secure session database: %w", err)
	}
	r.sessions = &threadStore{db: db, path: path, store: store}
	return r.sessions, nil
}

func (r *Runtime) closeThreadStore() error {
	r.sessionMu.Lock()
	store := r.sessions
	r.sessions = nil
	r.sessionMu.Unlock()
	if store == nil {
		return nil
	}
	if store.db == nil {
		return nil
	}
	return store.db.Close()
}

func defaultSessionDBPath(root string) (string, error) {
	path, err := datadir.SessionDBPath(os.Getenv)
	if err != nil {
		return "", err
	}
	if err := pathguard.ValidateOutside(path, root); err != nil {
		return "", fmt.Errorf("golem: validate session database: %w", err)
	}
	return path, nil
}
