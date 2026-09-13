package consult

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// inspection is a redacted local verdict over one consultant stream-json
// transcript, never a copy of vendor metadata. Every string field is a fixed
// literal chosen here, except Version (the init's declared CLI version) and
// Answer (the admitted reply text, control-sanitized). Fields are exported so
// the leak assertions in the tests can serialize the whole value.
type inspection struct {
	UnknownEvents      int      `json:"unknown_events"`
	UnknownKinds       []string `json:"unknown_kinds"`
	RateLimitEvents    int      `json:"rate_limit_events"`
	InitModelPresent   bool     `json:"init_model_present"`
	InitModelIsOpus    bool     `json:"init_model_is_opus"`
	ResponseModels     int      `json:"response_models"`
	ResponseOpusModels int      `json:"response_opus_models"`
	UsageModels        int      `json:"usage_models"`
	UsageOpusModels    int      `json:"usage_opus_models"`
	ReplyOK            bool     `json:"reply_ok"`
	InitCount          int      `json:"init_count"`
	ToolCount          int      `json:"tool_count"`
	MCPCount           int      `json:"mcp_count"`
	PluginCount        int      `json:"plugin_count"`
	ToolEvents         int      `json:"tool_events"`
	StartupEvents      int      `json:"startup_events"`
	TerminalCount      int      `json:"terminal_count"`
	ModelPresent       bool     `json:"model_present"`
	ModelIsOpus        bool     `json:"model_is_opus"`

	InitVersionExact           bool     `json:"init_version_exact"`
	Version                    string   `json:"version"`             // the init's claude_code_version, retained only as a bounded dotted-numeric literal
	InitAPIKeySource           string   `json:"init_api_key_source"` // none, env, helper, managed, legacy, missing, other
	InitPermissionMode         string   `json:"init_permission_mode"`
	InitInventoryPresent       bool     `json:"init_inventory_present"`
	SkillCount                 int      `json:"skill_count"`
	SlashCommandCount          int      `json:"slash_command_count"`
	AgentsPresent              bool     `json:"agents_present"`
	AgentCount                 int      `json:"agent_count"`
	AgentsAllDocumentedBuiltin bool     `json:"agents_all_documented_builtin"`
	InitCwdPresent             bool     `json:"init_cwd_present"`
	CwdMatches                 bool     `json:"cwd_matches"`
	SessionConsistent          bool     `json:"session_consistent"`
	UserEvents                 int      `json:"user_events"`
	PromptEqual                bool     `json:"prompt_equal"`
	NonNullParentRecords       int      `json:"non_null_parent_records"`
	AssistantErrorEvents       int      `json:"assistant_error_events"`
	APIRetryCount              int      `json:"api_retry_count"`
	APIRetryMaxAttempt         int      `json:"api_retry_max_attempt"`
	APIRetryErrors             []string `json:"api_retry_errors"` // pinned literals or other
	RateLimitTypes             []string `json:"rate_limit_types"` // pinned literals only
	OverageAttested            bool     `json:"overage_attested"`
	StatusEvents               int      `json:"status_events"`
	SessionStateEvents         int      `json:"session_state_events"`
	ThinkingTokenEvents        int      `json:"thinking_token_events"`
	TailEvents                 int      `json:"tail_events"`
	TaskEvents                 int      `json:"task_events"`
	NumTurns                   int64    `json:"num_turns"`
	StopReasonKind             string   `json:"stop_reason_kind"` // end_turn, null, other, missing
	DenialCount                int      `json:"denial_count"`
	DeferredToolUse            bool     `json:"deferred_tool_use"`
	UsageComplete              bool     `json:"usage_complete"`
	WebSearchZero              bool     `json:"web_search_zero"`
	UsageProvider              string   `json:"usage_provider"` // firstParty, other, missing, mixed
	OpusInputTokens            int64    `json:"opus_input_tokens"`
	OpusOutputTokens           int64    `json:"opus_output_tokens"`
	OpusCacheTokens            int64    `json:"opus_cache_tokens"`
	NonOpusInputTokens         int64    `json:"non_opus_input_tokens"`
	NonOpusOutputTokens        int64    `json:"non_opus_output_tokens"`
	NonOpusCacheTokens         int64    `json:"non_opus_cache_tokens"`
	DecodeErrorIndex           int      `json:"decode_error_index"` // -1 when the whole input decoded
	Answer                     string   `json:"answer"`             // the admitted reply, empty unless admission passed
}

// pinnedCLIVersion is the claude_code_version the frozen protocol catalog was
// recorded against; a different one is evidence, not a hard stop (the caller
// applies its own supported set to inspection.Version).
const pinnedCLIVersion = "2.1.240"

// versionRE bounds what may be copied out of the init into inspection.Version.
// The value reaches a caller that compares and reports it, so only a plain
// dotted-numeric literal survives; anything else leaves Version empty and is
// evidence solely through the pinned-version check.
var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// maxVersionLen bounds the retained version before it is matched.
const maxVersionLen = 32

// documentedBuiltinAgents is the pinned built-in subagent name list, taken
// from https://code.claude.com/docs/en/sub-agents ("Built-in subagents"),
// fetched 2026-09-12. Names are compared in memory only; the inspection
// retains a count and a boolean, never a name.
var documentedBuiltinAgents = map[string]bool{
	"Explore": true, "Plan": true, "general-purpose": true,
	"claude": true, "statusline-setup": true, "claude-code-guide": true,
}

// Fixed literal sets read from the pinned SDK declarations and the protocol
// catalog. Recognition never admits; it only names a bucket.
var (
	systemSubtypes = map[string]bool{
		"init": true, "compact_boundary": true, "status": true, "api_retry": true,
		"control_request_progress": true, "model_refusal_fallback": true, "model_refusal_no_fallback": true,
		"local_command_output": true, "hook_started": true, "hook_progress": true, "hook_response": true,
		"plugin_install": true, "task_notification": true, "task_started": true, "task_updated": true,
		"task_progress": true, "background_tasks_changed": true, "thinking_tokens": true,
		"session_state_changed": true, "worker_shutting_down": true, "commands_changed": true,
		"notification": true, "files_persisted": true, "memory_recall": true, "elicitation_complete": true,
		"permission_denied": true, "mirror_error": true, "informational": true, "turn_duration": true,
	}
	topTypes = map[string]bool{
		"user": true, "stream_event": true, "tool_progress": true, "auth_status": true,
		"tool_use_summary": true, "prompt_suggestion": true, "conversation_reset": true,
		"elicitation_request": true,
	}
	pinnedAssistantErrors = map[string]bool{
		"authentication_failed": true, "oauth_org_not_allowed": true, "account_on_hold": true,
		"billing_error": true, "rate_limit": true, "overloaded": true, "invalid_request": true,
		"model_not_found": true, "server_error": true, "unknown": true, "max_output_tokens": true,
	}
	pinnedRateLimitTypes = map[string]bool{
		"five_hour": true, "seven_day": true, "seven_day_opus": true, "seven_day_sonnet": true,
		"seven_day_overage_included": true, "overage": true,
	}
)

// initFacts is the redacted view of one system/init record. Every string is a
// fixed literal except version; sessionID stays in memory for the
// session-consistency check and is never serialized.
type initFacts struct {
	versionExact               bool
	version                    string
	apiKeySource               string // none, env, helper, managed, legacy, missing, other
	permissionMode             string // default, other, missing
	inventoryPresent           bool   // tools, mcp_servers, plugins, skills, slash_commands all arrays
	toolCount, mcpCount        int
	pluginCount, skillCount    int
	slashCount                 int
	agentsPresent              bool
	agentCount                 int
	agentsAllDocumentedBuiltin bool
	modelPresent, modelIsOpus  bool
	cwdPresent, cwdMatches     bool
	sessionID                  string
}

func isOpusModel(model string) bool {
	return model == "opus" || strings.HasPrefix(model, "claude-opus-")
}

// initCheck collects the launch-precondition facts of one init record; the
// caller decides which of them fail admission.
// expectedCwds lists the acceptable private cwd spellings (the envelope path
// and its symlink-resolved form); an empty list skips the cwd requirement.
func initCheck(m map[string]any, expectedCwds ...string) initFacts {
	var f initFacts
	if v, isString := m["claude_code_version"].(string); isString && len(v) <= maxVersionLen && versionRE.MatchString(v) {
		f.version = v
	}
	f.versionExact = m["claude_code_version"] == pinnedCLIVersion
	switch v, present := m["apiKeySource"]; {
	case !present:
		f.apiKeySource = "missing"
	case v == "none":
		f.apiKeySource = "none"
	case v == "ANTHROPIC_API_KEY":
		f.apiKeySource = "env"
	case v == "apiKeyHelper":
		f.apiKeySource = "helper"
	case v == "/login managed key":
		f.apiKeySource = "managed"
	case v == "user", v == "project", v == "org", v == "temporary", v == "oauth":
		f.apiKeySource = "legacy"
	default:
		f.apiKeySource = "other"
	}
	switch v, present := m["permissionMode"]; {
	case !present:
		f.permissionMode = "missing"
	case v == "default":
		f.permissionMode = "default"
	default:
		f.permissionMode = "other"
	}
	f.inventoryPresent = true
	for name, dst := range map[string]*int{"tools": &f.toolCount, "mcp_servers": &f.mcpCount, "plugins": &f.pluginCount, "skills": &f.skillCount, "slash_commands": &f.slashCount} {
		arr, ok := m[name].([]any)
		if !ok {
			f.inventoryPresent = false
			continue
		}
		*dst = len(arr)
	}
	if v, present := m["agents"]; present {
		f.agentsPresent = true
		arr, ok := v.([]any)
		f.agentsAllDocumentedBuiltin = ok
		f.agentCount = len(arr)
		for _, a := range arr {
			name, isString := a.(string)
			if !isString || !documentedBuiltinAgents[name] {
				f.agentsAllDocumentedBuiltin = false
			}
		}
	}
	if model, ok := m["model"].(string); ok && model != "" {
		f.modelPresent, f.modelIsOpus = true, isOpusModel(model)
	}
	if cwd, ok := m["cwd"].(string); ok && cwd != "" {
		f.cwdPresent = true
		for _, want := range expectedCwds {
			if want != "" && cwd == want {
				f.cwdMatches = true
			}
		}
	}
	f.sessionID, _ = m["session_id"].(string)
	return f
}

// inspector holds the in-memory stream state for one inspection.
type inspector struct {
	in             *inspection
	reasons        []string
	stdin          string   // post-substitution bytes written to the child
	cwds           []string // acceptable private cwd spellings; empty skips the check
	initSession    string
	sessionDrift   bool
	resultIndex    int
	firstAssistant int
	tailIdle       bool
	tailStatus     bool
	tailThinking   bool
	overageAll     bool
	resultText     string
	haveResult     bool
	allText        []string
	lastText       []string
}

// inspectStream applies the frozen stream-json admission catalog to one
// transcript and returns the redacted inspection plus the fixed-literal
// failure reasons, empty when the transcript is admissible. stdin is the
// post-substitution prompt used for the user-echo rule; cwds are the private
// working-directory spellings the init may declare (nil skips that check).
// It performs no I/O and retains no vendor text beyond the admitted answer.
func inspectStream(data []byte, stdin string, cwds []string) (inspection, []string) {
	// Empty slices serialize as [] rather than null so downstream gates can
	// test emptiness without a null special case.
	in := inspection{DecodeErrorIndex: -1, UnknownKinds: []string{}, APIRetryErrors: []string{}, RateLimitTypes: []string{}}
	x := &inspector{in: &in, stdin: stdin, cwds: cwds, resultIndex: -1, firstAssistant: -1, overageAll: true}
	if len(data) == 0 || len(data) > 1<<20 || !utf8.Valid(data) {
		x.fail("malformed")
		return in, x.reasons
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var records []map[string]any
	for {
		offset := d.InputOffset()
		v, err := claudeJSONValue(d, 0)
		if err == io.EOF && len(bytes.TrimSpace(data[offset:])) == 0 {
			break
		}
		m, isObject := v.(map[string]any)
		if err != nil || !isObject || len(records) >= 4096 {
			in.DecodeErrorIndex = len(records)
			x.fail("malformed")
			return in, x.reasons
		}
		records = append(records, m)
	}
	if len(records) == 0 {
		x.fail("malformed")
		return in, x.reasons
	}
	for i, m := range records {
		x.record(i, m)
	}
	if in.TerminalCount != 1 {
		x.fail("terminal-invalid")
	}
	if in.InitCount != 1 {
		x.fail("inventory-invalid")
	}
	in.SessionConsistent = in.InitCount == 1 && x.initSession != "" && !x.sessionDrift
	if in.InitCount == 1 && !in.SessionConsistent {
		x.fail("session-inconsistent")
	}
	in.OverageAttested = in.RateLimitEvents > 0 && x.overageAll
	// The reply is retained only when nothing else objected and the terminal
	// text is the text the assistant actually emitted.
	if x.haveResult && len(x.reasons) == 0 {
		want := strings.TrimRight(x.resultText, " \t\r\n")
		all := strings.TrimRight(strings.Join(x.allText, ""), " \t\r\n")
		last := strings.TrimRight(strings.Join(x.lastText, ""), " \t\r\n")
		if want != all && want != last {
			x.fail("answer-inconsistent")
		} else {
			in.Answer = sanitize(x.resultText)
		}
	}
	return in, x.reasons
}

// sanitize replaces C0 control bytes other than \n and \t, and DEL, with
// U+FFFD so a consultant cannot drive the terminal or the fence renderer.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			b.WriteRune('�')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// record dispatches one decoded record; every branch retains fixed literals.
func (x *inspector) record(i int, m map[string]any) {
	typ, _ := m["type"].(string)
	subtype, _ := m["subtype"].(string)
	if i > 0 {
		if sid, ok := m["session_id"].(string); !ok || sid == "" || sid != x.initSession {
			x.sessionDrift = true
		}
	}
	if x.resultIndex >= 0 && i > x.resultIndex {
		x.tail(m, typ, subtype)
		return
	}
	switch typ {
	case "result":
		x.result(i, m, subtype)
	case "system":
		x.system(i, m, subtype)
	case "assistant":
		x.assistant(i, m)
	case "user":
		x.user(m)
	case "rate_limit_event":
		x.rateLimit(m)
	case "tool_use", "tool_result", "tool_progress", "tool_use_summary":
		x.in.ToolEvents++
		x.fail("tool-activity")
	default:
		x.unknown("top", typ)
	}
}

// tail applies pre-registered rule 5: after the single success terminal only
// idle, a bare null status and thinking_tokens are admitted, each once.
func (x *inspector) tail(m map[string]any, typ, subtype string) {
	in := x.in
	if typ == "system" && envelopeOK(m) {
		switch subtype {
		case "session_state_changed":
			if m["state"] == "idle" && !x.tailIdle {
				x.tailIdle = true
				in.TailEvents++
				in.SessionStateEvents++
				return
			}
		case "status":
			v, present := m["status"]
			_, perm := m["permissionMode"]
			_, cr := m["compact_result"]
			_, ce := m["compact_error"]
			if present && v == nil && !perm && !cr && !ce && !x.tailStatus {
				x.tailStatus = true
				in.TailEvents++
				in.StatusEvents++
				return
			}
		case "thinking_tokens":
			if thinkingOK(m) && !x.tailThinking {
				x.tailThinking = true
				in.TailEvents++
				in.ThinkingTokenEvents++
				return
			}
		}
	}
	x.fail("tail-invalid")
}

func (x *inspector) result(i int, m map[string]any, subtype string) {
	in := x.in
	in.TerminalCount++
	if x.resultIndex < 0 {
		x.resultIndex = i
	}
	isError, explicit := m["is_error"].(bool)
	result, resultOK := m["result"].(string)
	in.ReplyOK = resultOK && strings.TrimSpace(result) == "OK"
	x.resultText, x.haveResult = result, resultOK
	terminalOK := true
	if n, ok := boundedInt(m["num_turns"]); ok {
		in.NumTurns = n
	} else {
		terminalOK = false
	}
	switch v, present := m["stop_reason"]; {
	case !present:
		in.StopReasonKind = "missing"
		terminalOK = false
		x.fail("stop-reason-invalid")
	case v == nil:
		in.StopReasonKind = "null"
		x.fail("stop-reason-invalid")
	case v == "end_turn":
		in.StopReasonKind = "end_turn"
	default:
		in.StopReasonKind = "other"
		if _, isString := v.(string); !isString {
			terminalOK = false
		}
		if v == "max_tokens" {
			x.fail("answer-truncated")
		} else {
			x.fail("stop-reason-invalid")
		}
	}
	for _, marker := range []string{"terminal_reason", "aborted", "supersedes"} {
		if _, present := m[marker]; present {
			x.fail("terminal-marker-present")
		}
	}
	if denials, ok := m["permission_denials"].([]any); ok {
		in.DenialCount += len(denials)
		if len(denials) != 0 {
			x.fail("denial-activity")
		}
	} else {
		terminalOK = false
	}
	if _, present := m["deferred_tool_use"]; present {
		in.DeferredToolUse = true
		x.fail("tool-activity")
	}
	if models, ok := m["modelUsage"].(map[string]any); ok && len(models) != 0 {
		x.usage(models)
	}
	if in.TerminalCount != 1 || subtype != "success" || !explicit || isError || !resultOK || strings.TrimSpace(result) == "" || !terminalOK {
		x.fail("terminal-invalid")
	}
}

// usage records per-model token totals. Missing fields stay unknown: they
// contribute nothing and clear UsageComplete/WebSearchZero.
func (x *inspector) usage(models map[string]any) {
	in := x.in
	in.ModelPresent, in.ModelIsOpus = true, true
	in.UsageModels += len(models)
	complete, zero := true, true
	first, missing, other := 0, 0, 0
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		opus := isOpusModel(name)
		if opus {
			in.UsageOpusModels++
		} else {
			in.ModelIsOpus = false
		}
		entry, ok := models[name].(map[string]any)
		if !ok {
			x.fail("usage-invalid")
			complete, zero = false, false
			missing++
			continue
		}
		var vals [5]int64
		for j, field := range []string{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens", "webSearchRequests"} {
			raw, present := entry[field]
			if !present {
				complete = false
				if field == "webSearchRequests" {
					zero = false
				}
				continue
			}
			n, ok := boundedInt(raw)
			if !ok {
				x.fail("usage-invalid")
				complete = false
				if field == "webSearchRequests" {
					zero = false
				}
				continue
			}
			vals[j] = n
		}
		if vals[4] != 0 {
			zero = false
			x.fail("web-search-activity")
		}
		if opus {
			in.OpusInputTokens += vals[0]
			in.OpusOutputTokens += vals[1]
			in.OpusCacheTokens += vals[2] + vals[3]
		} else {
			in.NonOpusInputTokens += vals[0]
			in.NonOpusOutputTokens += vals[1]
			in.NonOpusCacheTokens += vals[2] + vals[3]
		}
		switch p, present := entry["provider"]; {
		case !present:
			missing++
		case p == "firstParty":
			first++
		default:
			other++
			if _, isString := p.(string); isString {
				x.fail("route-invalid")
			} else {
				x.fail("usage-invalid")
			}
		}
	}
	in.UsageComplete = complete
	in.WebSearchZero = zero
	switch {
	case other > 0:
		in.UsageProvider = "other"
	case first > 0 && missing > 0:
		in.UsageProvider = "mixed"
	case first > 0:
		in.UsageProvider = "firstParty"
	default:
		in.UsageProvider = "missing"
	}
}

func (x *inspector) system(i int, m map[string]any, subtype string) {
	in := x.in
	switch subtype {
	case "init":
		in.InitCount++
		f := initCheck(m, x.cwds...)
		x.initSession = f.sessionID
		in.ToolCount += f.toolCount
		in.MCPCount += f.mcpCount
		in.PluginCount += f.pluginCount
		in.SkillCount += f.skillCount
		in.SlashCommandCount += f.slashCount
		in.InitVersionExact = f.versionExact
		in.Version = f.version
		in.InitAPIKeySource = f.apiKeySource
		in.InitPermissionMode = f.permissionMode
		in.InitInventoryPresent = f.inventoryPresent
		in.AgentsPresent, in.AgentCount, in.AgentsAllDocumentedBuiltin = f.agentsPresent, f.agentCount, f.agentsAllDocumentedBuiltin
		in.InitModelPresent, in.InitModelIsOpus = f.modelPresent, f.modelIsOpus
		in.InitCwdPresent, in.CwdMatches = f.cwdPresent, f.cwdMatches
		if in.InitCount != 1 || i != 0 || !f.inventoryPresent || f.toolCount+f.mcpCount+f.pluginCount+f.skillCount+f.slashCount != 0 {
			x.fail("inventory-invalid")
		}
		if !f.versionExact {
			x.fail("version-mismatch")
		}
		if !f.modelPresent || !f.modelIsOpus {
			x.fail("init-model-invalid")
		}
		if f.apiKeySource != "none" {
			x.fail("auth-source-invalid")
		}
		if f.permissionMode != "default" {
			x.fail("permission-mode-invalid")
		}
		if f.agentsPresent && !f.agentsAllDocumentedBuiltin {
			x.fail("agent-inventory-invalid")
		}
		if len(x.cwds) != 0 && !f.cwdMatches {
			x.fail("cwd-mismatch")
		}
	case "status":
		x.status(m)
	case "api_retry":
		x.apiRetry(m)
	case "session_state_changed":
		switch {
		case !envelopeOK(m):
			x.unknown("system", subtype)
		case m["state"] == "idle", m["state"] == "running":
			in.SessionStateEvents++
		default:
			x.fail("session-state-invalid")
		}
	case "thinking_tokens":
		if envelopeOK(m) && thinkingOK(m) {
			in.ThinkingTokenEvents++
		} else {
			x.unknown("system", subtype)
		}
	case "hook_started", "hook_progress", "hook_response", "plugin_install":
		in.StartupEvents++
		x.fail("startup-activity")
	case "task_notification", "task_started", "task_updated", "task_progress", "background_tasks_changed":
		in.TaskEvents++
		x.fail("task-activity")
	case "memory_recall":
		x.fail("memory-activity")
	case "elicitation_complete":
		x.fail("elicitation-activity")
	case "model_refusal_fallback", "model_refusal_no_fallback":
		x.fail("fallback-activity")
	case "compact_boundary":
		x.fail("compaction-activity")
	default:
		x.unknown("system", subtype)
	}
}

// status applies pre-registered rule 6 before the terminal.
func (x *inspector) status(m map[string]any) {
	v, present := m["status"]
	if !present || !envelopeOK(m) {
		x.unknown("system", "status")
		return
	}
	switch v {
	case nil, "requesting":
	case "compacting":
		x.fail("compaction-activity")
		return
	default:
		x.unknown("system", "status")
		return
	}
	if pm, present := m["permissionMode"]; present && pm != "default" {
		x.fail("permission-mode-changed")
	}
	_, cr := m["compact_result"]
	_, ce := m["compact_error"]
	if cr || ce {
		x.fail("compaction-activity")
	}
	x.in.StatusEvents++
}

// apiRetry applies pre-registered rule 2.
func (x *inspector) apiRetry(m map[string]any) {
	in := x.in
	in.APIRetryCount++
	valid := envelopeOK(m)
	attempt, attemptOK := boundedInt(m["attempt"])
	if attemptOK {
		if int(attempt) > in.APIRetryMaxAttempt {
			in.APIRetryMaxAttempt = int(attempt)
		}
	} else {
		valid = false
	}
	for _, field := range []string{"max_retries", "retry_delay_ms"} {
		if _, fieldOK := boundedInt(m[field]); !fieldOK {
			valid = false
		}
	}
	if v, present := m["error_status"]; !present {
		valid = false
	} else if v != nil {
		if _, isNumber := v.(json.Number); !isNumber {
			valid = false
		}
	}
	lit, isString := m["error"].(string)
	if !isString {
		valid = false
	}
	if !valid {
		x.fail("api-retry-invalid")
	}
	if isString {
		recorded := "other"
		if pinnedAssistantErrors[lit] {
			recorded = lit
		}
		if len(in.APIRetryErrors) < 8 {
			in.APIRetryErrors = append(in.APIRetryErrors, recorded)
		}
		switch lit {
		case "overloaded", "server_error", "rate_limit":
		default:
			x.fail("api-retry-" + recorded)
		}
	}
	if (attemptOK && attempt > 2) || in.APIRetryCount > 2 {
		x.fail("api-retry-excess")
	}
}

// rateLimit keeps the envelope checks and adds pre-registered rule 3.
func (x *inspector) rateLimit(m map[string]any) {
	in := x.in
	in.RateLimitEvents++
	info, ok := m["rate_limit_info"].(map[string]any)
	if in.InitCount != 1 || !ok || !envelopeOK(m) {
		x.fail("rate-limit-invalid")
		x.overageAll = false
		return
	}
	switch info["status"] {
	case "allowed", "allowed_warning":
	case "rejected":
		x.fail("quota-rejected")
	default:
		x.fail("rate-limit-invalid")
	}
	for _, field := range []string{"resetsAt", "utilization", "overageResetsAt", "surpassedThreshold"} {
		if v, exists := info[field]; exists {
			if _, isNumber := v.(json.Number); !isNumber {
				x.fail("rate-limit-invalid")
			}
		}
	}
	if v, exists := info["overageStatus"]; exists && v != "rejected" {
		x.fail("overage-not-rejected")
	}
	if v, exists := info["overageDisabledReason"]; exists {
		if _, isString := v.(string); !isString {
			x.fail("rate-limit-invalid")
		}
	}
	for _, field := range []string{"canUserPurchaseCredits", "hasChargeableSavedPaymentMethod"} {
		if v, exists := info[field]; exists {
			if _, isBool := v.(bool); !isBool {
				x.fail("rate-limit-invalid")
			}
		}
	}
	attested := true
	for _, field := range []string{"isUsingOverage", "overageInUse"} {
		v, exists := info[field]
		if !exists {
			attested = false
			continue
		}
		flag, isBool := v.(bool)
		switch {
		case !isBool:
			x.fail("rate-limit-invalid")
			attested = false
		case flag:
			x.fail("overage-in-use")
			attested = false
		}
	}
	if v, exists := info["rateLimitType"]; exists {
		name, isString := v.(string)
		switch {
		case !isString || !pinnedRateLimitTypes[name]:
			x.fail("rate-limit-invalid")
		case name == "overage":
			x.fail("overage-in-use")
			in.RateLimitTypes = appendUnique(in.RateLimitTypes, name)
		default:
			in.RateLimitTypes = appendUnique(in.RateLimitTypes, name)
		}
	}
	if v, exists := info["errorCode"]; exists {
		if v == "credits_required" {
			x.fail("credits-required")
		} else {
			x.fail("rate-limit-invalid")
		}
		attested = false
	}
	if !attested {
		x.overageAll = false
	}
}

func (x *inspector) assistant(i int, m map[string]any) {
	in := x.in
	if in.InitCount != 1 {
		x.unknown("assistant", "envelope")
		return
	}
	if x.firstAssistant < 0 {
		x.firstAssistant = i
	}
	x.lastText = nil
	if v, present := m["parent_tool_use_id"]; !present {
		x.unknown("assistant", "envelope")
	} else if v != nil {
		in.NonNullParentRecords++
		x.fail("subagent-activity")
	}
	if _, present := m["error"]; present {
		in.AssistantErrorEvents++
		x.fail("assistant-error")
	}
	if _, present := m["supersedes"]; present {
		x.fail("terminal-marker-present")
	}
	message, ok := m["message"].(map[string]any)
	if !ok {
		x.unknown("assistant", "content")
		return
	}
	in.ResponseModels++
	if model, ok := message["model"].(string); ok && isOpusModel(model) {
		in.ResponseOpusModels++
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) == 0 {
		x.unknown("assistant", "content")
		return
	}
	for _, value := range content {
		block, ok := value.(map[string]any)
		if !ok {
			x.unknown("assistant", "content")
			continue
		}
		kind, _ := block["type"].(string)
		field := ""
		switch kind {
		case "text":
			field = "text"
		case "thinking":
			field = "thinking"
		case "redacted_thinking":
			field = "data"
		case "tool_use", "server_tool_use", "tool_result":
			in.ToolEvents++
			x.fail("tool-activity")
		default:
			x.unknown("assistant", "content")
		}
		if field == "" {
			continue
		}
		text, isString := block[field].(string)
		if !isString {
			x.unknown("assistant", "content")
			continue
		}
		// Only text blocks are retained; thinking is read and dropped.
		if kind == "text" {
			x.allText = append(x.allText, text)
			x.lastText = append(x.lastText, text)
		}
	}
}

// user applies pre-registered rule 1: one byte-exact prompt echo before the
// first assistant record, with every exclusion field absent.
func (x *inspector) user(m map[string]any) {
	in := x.in
	in.UserEvents++
	if in.UserEvents > 1 {
		in.PromptEqual = false // a second user event withdraws the first echo's admission
	}
	ok := in.InitCount == 1 && in.UserEvents == 1 && x.firstAssistant < 0
	if v, present := m["parent_tool_use_id"]; !present || v != nil {
		ok = false
	}
	for _, field := range []string{"isReplay", "tool_use_result", "isSynthetic", "origin", "priority", "shouldQuery", "file_attachments"} {
		if _, present := m[field]; present {
			ok = false
		}
	}
	message, isMap := m["message"].(map[string]any)
	if !isMap || message["role"] != "user" {
		ok = false
	}
	text, haveText := "", false
	if isMap {
		switch c := message["content"].(type) {
		case string:
			text, haveText = c, true
		case []any:
			if len(c) == 1 {
				if block, isBlock := c[0].(map[string]any); isBlock && block["type"] == "text" {
					text, haveText = block["text"].(string)
				}
			}
		}
	}
	// Byte-for-byte against the post-substitution stdin, no trimming.
	if ok && haveText && x.stdin != "" && text == x.stdin {
		in.PromptEqual = true
		return
	}
	x.fail("user-event-invalid")
}

// unknown records a fixed bucket and fails admission. Recognition here never
// admits an event, and no vendor discriminator survives.
func (x *inspector) unknown(location, kind string) {
	switch location {
	case "system":
		if !systemSubtypes[kind] {
			kind = "other"
		}
	case "top":
		if !topTypes[kind] {
			kind = "other"
		}
	case "assistant":
		if kind != "content" && kind != "envelope" {
			kind = "other"
		}
	}
	x.fail("unknown-event")
	x.in.UnknownKinds = appendUnique(x.in.UnknownKinds, location+":"+kind)
}

// fail records one fixed local literal; never pass a vendor string.
func (x *inspector) fail(reason string) {
	if reason == "unknown-event" {
		x.in.UnknownEvents++
	}
	x.reasons = appendUnique(x.reasons, reason)
}

func appendUnique(list []string, item string) []string {
	for _, old := range list {
		if old == item {
			return list
		}
	}
	return append(list, item)
}

// envelopeOK requires the non-empty uuid and session_id every catalog system
// event declares.
func envelopeOK(m map[string]any) bool {
	uuid, uok := m["uuid"].(string)
	session, sok := m["session_id"].(string)
	return uok && uuid != "" && sok && session != ""
}

func thinkingOK(m map[string]any) bool {
	_, a := boundedInt(m["estimated_tokens"])
	_, b := boundedInt(m["estimated_tokens_delta"])
	return a && b
}

// boundedInt accepts only a nonnegative integer literal at most 2^53.
func boundedInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	s := n.String()
	if strings.ContainsAny(s, ".eE") {
		return 0, false
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil || i < 0 || i > 1<<53 {
		return 0, false
	}
	return i, true
}

// Decode token-by-token because encoding/json's ordinary map/struct decode
// silently accepts duplicate object keys. Depth and total input are bounded.
func claudeJSONValue(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("depth")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, container := t.(json.Delim)
	if !container {
		return t, nil
	}
	switch delim {
	case '{':
		m := make(map[string]any)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, errors.New("key")
			}
			if _, duplicate := m[name]; duplicate {
				return nil, errors.New("duplicate")
			}
			value, err := claudeJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[name] = value
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("object")
		}
		return m, nil
	case '[':
		a := make([]any, 0)
		for d.More() {
			value, err := claudeJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, value)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("array")
		}
		return a, nil
	default:
		return nil, errors.New("delimiter")
	}
}
