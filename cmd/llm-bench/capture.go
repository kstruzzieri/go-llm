package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/conversation"

	_ "modernc.org/sqlite"
)

const defaultCaptureSource = "conversation-store"

var (
	errConversationMissingID        = errors.New("conversation has empty id")
	errConversationMissingSystem    = errors.New("conversation has no system message")
	errConversationMissingUser      = errors.New("conversation has no user turn")
	errConversationMissingAssistant = errors.New("conversation has no final assistant answer")
	errConversationInvalidToolCall  = errors.New("conversation has invalid tool call")
)

type captureOptions struct {
	DBPath           string
	OutputDir        string
	Limit            int
	Source           string
	FallbackSystem   string
	ToolSchemaSource toolSchemaSource // nil = no snapshot; configured empty snapshot is authoritative
}

type captureResult struct {
	Written []string
	Skipped []string
}

type conversationStore interface {
	List(ctx context.Context) ([]conversation.Summary, error)
	Load(ctx context.Context, id string) (*conversation.Conversation, error)
}

func runCapture(ctx context.Context, opts captureOptions) (captureResult, error) {
	if strings.TrimSpace(opts.DBPath) == "" {
		return captureResult{}, fmt.Errorf("capture db path is required")
	}
	info, err := os.Stat(opts.DBPath)
	if err != nil {
		return captureResult{}, fmt.Errorf("stat capture db %q: %w", opts.DBPath, err)
	}
	if info.IsDir() {
		return captureResult{}, fmt.Errorf("capture db %q is a directory", opts.DBPath)
	}

	db, err := openReadOnlySQLite(opts.DBPath)
	if err != nil {
		return captureResult{}, fmt.Errorf("open capture db %q: %w", opts.DBPath, err)
	}
	defer func() { _ = db.Close() }()

	return captureConversations(ctx, &captureConversationReader{db: db}, opts)
}

func openReadOnlySQLite(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

type captureConversationReader struct {
	db *sql.DB
}

func (r *captureConversationReader) List(ctx context.Context) ([]conversation.Summary, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id,
		        title,
		        CASE WHEN json_valid(messages) THEN json_array_length(messages) ELSE 0 END,
		        created_at,
		        updated_at
		   FROM conversations
		  ORDER BY updated_at DESC, id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("capture: list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var summaries []conversation.Summary
	for rows.Next() {
		var s conversation.Summary
		var createdMs, updatedMs int64
		if err := rows.Scan(&s.ID, &s.Title, &s.MessageCount, &createdMs, &updatedMs); err != nil {
			return nil, fmt.Errorf("capture: scan conversation summary: %w", err)
		}
		s.CreatedAt = time.UnixMilli(createdMs)
		s.UpdatedAt = time.UnixMilli(updatedMs)
		summaries = append(summaries, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("capture: iterate conversation summaries: %w", err)
	}
	if summaries == nil {
		return []conversation.Summary{}, nil
	}
	return summaries, nil
}

func (r *captureConversationReader) Load(ctx context.Context, id string) (*conversation.Conversation, error) {
	var conv conversation.Conversation
	var messagesJSON string
	var createdMs, updatedMs int64

	err := r.db.QueryRowContext(ctx,
		`SELECT id, title, messages, created_at, updated_at FROM conversations WHERE id = ?`,
		id,
	).Scan(&conv.ID, &conv.Title, &messagesJSON, &createdMs, &updatedMs)
	if err != nil {
		return nil, fmt.Errorf("capture: load conversation %q: %w", id, err)
	}
	if err := json.Unmarshal([]byte(messagesJSON), &conv.Messages); err != nil {
		return nil, fmt.Errorf("capture: load conversation %q: unmarshal messages: %w", id, err)
	}
	conv.CreatedAt = time.UnixMilli(createdMs)
	conv.UpdatedAt = time.UnixMilli(updatedMs)
	return &conv, nil
}

func captureConversations(ctx context.Context, store conversationStore, opts captureOptions) (captureResult, error) {
	outDir := strings.TrimSpace(opts.OutputDir)
	if outDir == "" {
		outDir = filepath.Join("docs", "llm", "traces")
	}
	source := strings.TrimSpace(opts.Source)
	if source == "" {
		source = defaultCaptureSource
	}

	summaries, err := store.List(ctx)
	if err != nil {
		return captureResult{}, err
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		if !summaries[i].UpdatedAt.Equal(summaries[j].UpdatedAt) {
			return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
		}
		return summaries[i].ID < summaries[j].ID
	})
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return captureResult{}, fmt.Errorf("create trace dir %q: %w", outDir, err)
	}

	redactor := defaultRedactor()
	snapshotConfigured := opts.ToolSchemaSource != nil
	var snapshotTools []json.RawMessage
	if snapshotConfigured {
		tools, err := opts.ToolSchemaSource.Snapshot(ctx)
		if err != nil {
			return captureResult{}, fmt.Errorf("capture: tool schema snapshot: %w", err)
		}
		snapshotTools, err = redactToolSchemas(tools, redactor)
		if err != nil {
			return captureResult{}, fmt.Errorf("capture: redact tool schema snapshot: %w", err)
		}
	}

	var result captureResult
	for _, summary := range summaries {
		if opts.Limit > 0 && len(result.Written) >= opts.Limit {
			break
		}

		conv, err := store.Load(ctx, summary.ID)
		if err != nil {
			result.Skipped = append(result.Skipped, fmt.Sprintf("%s: %v", summary.ID, err))
			continue
		}

		trace, err := conversationToTrace(*conv, source, opts.FallbackSystem, redactor, snapshotTools, snapshotConfigured)
		if err != nil {
			if errors.Is(err, errToolNameNotDeclared) {
				name := undeclaredToolName(err)
				result.Skipped = append(result.Skipped, fmt.Sprintf("%s: tool %q not in MCP snapshot", summary.ID, name))
			} else {
				result.Skipped = append(result.Skipped, fmt.Sprintf("%s: %v", summary.ID, err))
			}
			continue
		}

		path := filepath.Join(outDir, safeTraceFilename(trace.ID)+".json")
		data, err := json.MarshalIndent(trace, "", "  ")
		if err != nil {
			return result, fmt.Errorf("marshal trace %q: %w", trace.ID, err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return result, fmt.Errorf("write trace %q: %w", path, err)
		}
		result.Written = append(result.Written, path)
	}

	return result, nil
}

func conversationToTrace(conv conversation.Conversation, source, fallbackSystem string, redactor redactor, tools []json.RawMessage, schemaSnapshotConfigured bool) (Trace, error) {
	if strings.TrimSpace(conv.ID) == "" {
		return Trace{}, errConversationMissingID
	}

	system, turns, finalAnswer, err := conversationTurns(conv.Messages, fallbackSystem, redactor)
	if err != nil {
		return Trace{}, err
	}

	if tools == nil {
		tools = []json.RawMessage{}
	}

	trace := Trace{
		ID:         "conversation-" + conv.ID,
		Source:     source,
		CapturedAt: captureTime(conv),
		System:     system,
		Tools:      tools,
		Turns:      turns,
		Golden: Golden{
			ToolCalls:            goldenToolCalls(turns),
			FinalAnswerCriteria:  "Assistant answer should satisfy the captured final assistant response.",
			FinalAnswerSubstring: finalAnswer,
		},
	}
	if schemaSnapshotConfigured && len(trace.Tools) == 0 && len(trace.Golden.ToolCalls) > 0 {
		return Trace{}, fmt.Errorf("tool %q: %w", trace.Golden.ToolCalls[0], errToolNameNotDeclared)
	}
	if err := validateTrace(trace); err != nil {
		return Trace{}, err
	}
	return trace, nil
}

func conversationTurns(messages []conversation.Message, fallbackSystem string, redactor redactor) (string, []Turn, string, error) {
	var system string
	var turns []Turn
	sawUser := false
	finalAssistantIndex, finalAnswer := capturedFinalAnswer(messages, redactor)

	for i, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}

		if role == "system" {
			if system == "" {
				system = redactor.Redact(msg.Content)
			}
			continue
		}
		if i == finalAssistantIndex {
			continue
		}

		turn := Turn{
			Role:       role,
			Content:    redactor.Redact(msg.Content),
			ToolCallID: redactor.Redact(msg.ToolCallID),
			Name:       redactor.Redact(msg.ToolName),
		}
		if len(msg.ToolCalls) > 0 {
			toolCalls, err := convertToolCalls(msg.ToolCalls, redactor)
			if err != nil {
				return "", nil, "", err
			}
			turn.ToolCalls = toolCalls
		}
		if role == "user" {
			sawUser = true
		}
		turns = append(turns, turn)
	}

	if system == "" {
		system = redactor.Redact(strings.TrimSpace(fallbackSystem))
	}
	if system == "" {
		return "", nil, "", errConversationMissingSystem
	}
	if !sawUser {
		return "", nil, "", errConversationMissingUser
	}
	if strings.TrimSpace(finalAnswer) == "" {
		return "", nil, "", errConversationMissingAssistant
	}

	return system, turns, finalAnswer, nil
}

func capturedFinalAnswer(messages []conversation.Message, redactor redactor) (int, string) {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(messages[i].Role) == "assistant" && strings.TrimSpace(messages[i].Content) != "" {
			return i, redactor.Redact(messages[i].Content)
		}
	}
	return -1, ""
}

type storedToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Function  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func convertToolCalls(raw json.RawMessage, redactor redactor) ([]ToolCall, error) {
	var stored []storedToolCall
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("%w: %v", errConversationInvalidToolCall, err)
	}

	toolCalls := make([]ToolCall, 0, len(stored))
	for _, call := range stored {
		name := strings.TrimSpace(call.Name)
		args := call.Arguments
		if name == "" {
			name = strings.TrimSpace(call.Function.Name)
			args = call.Function.Arguments
		}
		if name == "" {
			return nil, fmt.Errorf("%w: missing name", errConversationInvalidToolCall)
		}

		redactedArgs, err := redactRawJSON(args, redactor)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errConversationInvalidToolCall, err)
		}
		toolCalls = append(toolCalls, ToolCall{
			Name:      redactor.Redact(name),
			Arguments: redactedArgs,
		})
	}

	return toolCalls, nil
}

func redactRawJSON(raw json.RawMessage, redactor redactor) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(`{}`), nil
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	value = redactJSONValue(value, redactor)

	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func redactToolSchemas(tools []json.RawMessage, redactor redactor) ([]json.RawMessage, error) {
	if len(tools) == 0 {
		return []json.RawMessage{}, nil
	}
	out := make([]json.RawMessage, len(tools))
	for i, raw := range tools {
		redacted, err := redactToolSchema(raw, redactor)
		if err != nil {
			return nil, fmt.Errorf("tool[%d]: %w", i, err)
		}
		out[i] = redacted
	}
	return out, nil
}

func redactToolSchema(raw json.RawMessage, redactor redactor) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(`{}`), nil
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	value = redactSchemaJSONValue(value, redactor, false)

	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func redactSchemaJSONValue(value any, redactor redactor, sensitiveProperty bool) any {
	switch v := value.(type) {
	case map[string]any:
		for key, nested := range v {
			keySensitive := sensitiveProperty || isSensitiveKey(key)
			if isSchemaPatternKey(key) {
				if pattern, ok := nested.(string); ok {
					v[key] = redactRegexString(pattern, redactor)
					continue
				}
			}
			if isSchemaPatternPropertiesKey(key) {
				if patterns, ok := nested.(map[string]any); ok {
					v[key] = redactPatternProperties(patterns, redactor, keySensitive)
					continue
				}
			}
			if sensitiveProperty && isSensitiveSchemaValueKey(key) {
				v[key] = redactSensitiveSchemaValue(nested, redactor)
				continue
			}
			redacted := redactSchemaJSONValue(nested, redactor, keySensitive)
			if _, ok := redacted.(string); ok && isSensitiveKey(key) {
				v[key] = redactor.secretPlaceholder
				continue
			}
			v[key] = redacted
		}
		return v
	case []any:
		for i, nested := range v {
			v[i] = redactSchemaJSONValue(nested, redactor, sensitiveProperty)
		}
		return v
	case string:
		return redactor.Redact(v)
	default:
		return value
	}
}

func redactSensitiveSchemaValue(value any, redactor redactor) any {
	switch v := value.(type) {
	case map[string]any:
		for key, nested := range v {
			v[key] = redactSensitiveSchemaValue(nested, redactor)
		}
		return v
	case []any:
		for i, nested := range v {
			v[i] = redactSensitiveSchemaValue(nested, redactor)
		}
		return v
	case string:
		return redactor.secretPlaceholder
	default:
		return value
	}
}

func redactPatternProperties(patterns map[string]any, redactor redactor, sensitiveProperty bool) map[string]any {
	redacted := make(map[string]any, len(patterns))
	for pattern, schema := range patterns {
		redacted[redactRegexString(pattern, redactor)] = redactSchemaJSONValue(schema, redactor, sensitiveProperty || isSensitiveKey(pattern))
	}
	return redacted
}

func redactRegexString(pattern string, redactor redactor) string {
	redacted := redactor.Redact(pattern)
	for _, placeholder := range []string{redactor.pathPlaceholder, redactor.secretPlaceholder, redactor.emailPlaceholder} {
		redacted = strings.ReplaceAll(redacted, placeholder, regexp.QuoteMeta(placeholder))
	}
	return redacted
}

func isSensitiveSchemaValueKey(key string) bool {
	switch normalizeSchemaKey(key) {
	case "default", "examples", "enum", "const":
		return true
	default:
		return false
	}
}

func isSchemaPatternKey(key string) bool {
	return normalizeSchemaKey(key) == "pattern"
}

func isSchemaPatternPropertiesKey(key string) bool {
	return normalizeSchemaKey(key) == "pattern_properties"
}

func normalizeSchemaKey(key string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
}

func redactJSONValue(value any, redactor redactor) any {
	switch v := value.(type) {
	case map[string]any:
		for key, nested := range v {
			redacted := redactJSONValue(nested, redactor)
			if _, ok := redacted.(string); ok && isSensitiveKey(key) {
				v[key] = redactor.secretPlaceholder
				continue
			}
			v[key] = redacted
		}
		return v
	case []any:
		for i, nested := range v {
			v[i] = redactJSONValue(nested, redactor)
		}
		return v
	case string:
		return redactor.Redact(v)
	default:
		return value
	}
}

func goldenToolCalls(turns []Turn) []string {
	var names []string
	for _, turn := range turns {
		for _, call := range turn.ToolCalls {
			names = append(names, call.Name)
		}
	}
	if names == nil {
		return []string{}
	}
	return names
}

func finalAssistantAnswer(turns []Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" {
			return turns[i].Content
		}
	}
	return ""
}

func captureTime(conv conversation.Conversation) time.Time {
	if !conv.UpdatedAt.IsZero() {
		return conv.UpdatedAt.UTC()
	}
	if !conv.CreatedAt.IsZero() {
		return conv.CreatedAt.UTC()
	}
	return time.Unix(0, 0).UTC()
}

func safeTraceFilename(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "trace"
	}
	return name
}

type redactor struct {
	pathPlaceholder   string
	secretPlaceholder string
	emailPlaceholder  string
}

func defaultRedactor() redactor {
	return redactor{
		pathPlaceholder:   "[REDACTED_PATH]",
		secretPlaceholder: "[REDACTED_SECRET]",
		emailPlaceholder:  "[REDACTED_EMAIL]",
	}
}

var (
	absolutePathPattern  = regexp.MustCompile(`(?i)(?:/(?:Users|home|private|var|tmp|Volumes|opt|etc|usr|workspace|mnt)/[^\s,'"()]+)|(?:[A-Z]:\\[^\s,'"()]+)`)
	secretPattern        = regexp.MustCompile(`(?i)(["']?[A-Z0-9_./:-]*(?:api[_-]?key|apikey|auth[_-]?token|access[_-]?token|refresh[_-]?token|id[_-]?token|token|secret|password|passwd|private[_-]?key|authorization)["']?\s*[:=]\s*)(["']?)([^"',\s;&}]+)(["']?)`)
	authHeaderPattern    = regexp.MustCompile(`(?i)\bAuthorization\s*:\s*[^\s,;&]+(?:\s+[^\s,;&]+)?`)
	bearerPattern        = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
	tokenValuePattern    = regexp.MustCompile(`(?i)\b(?:sk-[A-Za-z0-9][A-Za-z0-9_-]{16,}|ghp_[A-Za-z0-9_]{16,}|github_pat_[A-Za-z0-9_]{16,}|glpat-[A-Za-z0-9_-]{16,}|xox[baprs]-[A-Za-z0-9-]{10,}|npm_[A-Za-z0-9]{16,})\b`)
	urlCredentialPattern = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/\s:@]+):([^@\s/]+)@`)
	privateKeyPattern    = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	emailPattern         = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
)

func (r redactor) Redact(s string) string {
	if s == "" {
		return ""
	}
	s = privateKeyPattern.ReplaceAllString(s, r.secretPlaceholder)
	s = absolutePathPattern.ReplaceAllStringFunc(s, func(match string) string {
		base := pathBase(match)
		if base == "" {
			return r.pathPlaceholder
		}
		return r.pathPlaceholder + "/" + base
	})
	s = urlCredentialPattern.ReplaceAllString(s, "${1}"+r.secretPlaceholder+"@")
	s = authHeaderPattern.ReplaceAllString(s, "Authorization: "+r.secretPlaceholder)
	s = bearerPattern.ReplaceAllString(s, "Bearer "+r.secretPlaceholder)
	s = secretPattern.ReplaceAllStringFunc(s, func(match string) string {
		parts := secretPattern.FindStringSubmatch(match)
		if len(parts) != 5 {
			return r.secretPlaceholder
		}
		return parts[1] + parts[2] + r.secretPlaceholder + parts[4]
	})
	s = tokenValuePattern.ReplaceAllString(s, r.secretPlaceholder)
	s = emailPattern.ReplaceAllString(s, r.emailPlaceholder)
	return s
}

func pathBase(path string) string {
	path = strings.TrimRight(path, `.,;:`)
	idx := strings.LastIndexAny(path, `/\`)
	if idx < 0 || idx == len(path)-1 {
		return ""
	}
	return path[idx+1:]
}

// undeclaredToolName recovers the offending tool name embedded in the
// wrapped error chain by validateToolNamesDeclared. Returns "" if the
// chain doesn't include a quoted name (defensive — the validator
// always quotes the name today).
func undeclaredToolName(err error) string {
	msg := err.Error()
	first := strings.Index(msg, `"`)
	if first < 0 {
		return ""
	}
	last := strings.Index(msg[first+1:], `"`)
	if last < 0 {
		return ""
	}
	return msg[first+1 : first+1+last]
}

func isSensitiveKey(key string) bool {
	k := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	return strings.Contains(k, "token") ||
		strings.Contains(k, "secret") ||
		strings.Contains(k, "password") ||
		strings.Contains(k, "passwd") ||
		strings.Contains(k, "private_key") ||
		strings.Contains(k, "privatekey") ||
		strings.Contains(k, "api_key") ||
		(strings.Contains(k, "api") && strings.Contains(k, "key")) ||
		(strings.Contains(k, "access") && strings.Contains(k, "key")) ||
		k == "apikey" ||
		k == "authorization"
}
