package consult

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Stream-json admission tests ported from the frozen E2.2 harness (53 tests /
// 47 mutations). All fixtures are synthetic; none is captured vendor output.
// Every serialized inspection is checked for leaked payload.

// validInit satisfies every launch precondition with synthetic identifiers.
const validInit = `{"type":"system","subtype":"init","claude_code_version":"2.1.240","apiKeySource":"none","permissionMode":"default","tools":[],"mcp_servers":[],"plugins":[],"skills":[],"slash_commands":[],"model":"claude-opus-4-8","cwd":"/private/synthetic","session_id":"synthetic-session","uuid":"synthetic-uuid"}`
const goodResult = `{"type":"result","subtype":"success","is_error":false,"result":"OK","num_turns":1,"stop_reason":"end_turn","permission_denials":[],"session_id":"synthetic-session","uuid":"synthetic-uuid"}`

// assistantOK is one admissible text block with the explicit null parent. Its
// text equals goodResult's result so the answer-consistency rule admits it.
const assistantOK = `{"type":"assistant","parent_tool_use_id":null,"session_id":"synthetic-session","uuid":"synthetic-uuid","message":{"model":"claude-opus-4-8","role":"assistant","content":[{"type":"text","text":"OK"}]}}`

func withUsage(usage string) string {
	return strings.Replace(goodResult, `"result":"OK"`, `"result":"OK","modelUsage":`+usage, 1)
}

func stream(records ...string) string { return strings.Join(records, "\n") + "\n" }

func sysEvent(subtype, extra string) string {
	return `{"type":"system","subtype":"` + subtype + `","uuid":"synthetic-uuid","session_id":"synthetic-session"` + extra + `}`
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func ok(reasons []string) bool { return len(reasons) == 0 }

func mustNotLeak(t *testing.T, in inspection, reasons []string, needles ...string) {
	t.Helper()
	b, err := json.Marshal(struct {
		In      inspection `json:"inspection"`
		Reasons []string   `json:"reasons"`
	}{in, reasons})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range append(needles, "SENSITIVE") {
		if n != "" && strings.Contains(string(b), n) {
			t.Fatalf("inspection leaked %q: %s", n, b)
		}
	}
}

func TestClaudeInitRequiresPinnedVersionAuthSourcePermissionAndInventory(t *testing.T) {
	for _, tc := range []struct {
		name, init, reason string
	}{
		{"valid", validInit, ""},
		{"version", strings.Replace(validInit, "2.1.240", "2.1.241", 1), "version-mismatch"},
		{"version_missing", strings.Replace(validInit, `"claude_code_version":"2.1.240",`, ``, 1), "version-mismatch"},
		{"api_key_env", strings.Replace(validInit, `"apiKeySource":"none"`, `"apiKeySource":"ANTHROPIC_API_KEY"`, 1), "auth-source-invalid"},
		{"api_key_legacy", strings.Replace(validInit, `"apiKeySource":"none"`, `"apiKeySource":"oauth"`, 1), "auth-source-invalid"},
		{"api_key_missing", strings.Replace(validInit, `"apiKeySource":"none",`, ``, 1), "auth-source-invalid"},
		{"api_key_other", strings.Replace(validInit, `"apiKeySource":"none"`, `"apiKeySource":"SENSITIVE"`, 1), "auth-source-invalid"},
		{"permission", strings.Replace(validInit, `"permissionMode":"default"`, `"permissionMode":"plan"`, 1), "permission-mode-invalid"},
		{"permission_missing", strings.Replace(validInit, `"permissionMode":"default",`, ``, 1), "permission-mode-invalid"},
		{"skills", strings.Replace(validInit, `"skills":[]`, `"skills":["SENSITIVE"]`, 1), "inventory-invalid"},
		{"skills_missing", strings.Replace(validInit, `"skills":[],`, ``, 1), "inventory-invalid"},
		{"slash", strings.Replace(validInit, `"slash_commands":[]`, `"slash_commands":["/SENSITIVE"]`, 1), "inventory-invalid"},
		{"agents_builtin", strings.Replace(validInit, `"tools":[]`, `"tools":[],"agents":["Explore","Plan","general-purpose"]`, 1), ""},
		{"agents_unknown", strings.Replace(validInit, `"tools":[]`, `"tools":[],"agents":["Explore","SENSITIVE"]`, 1), "agent-inventory-invalid"},
		{"agents_not_array", strings.Replace(validInit, `"tools":[]`, `"tools":[],"agents":"Explore"`, 1), "agent-inventory-invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(stream(tc.init, assistantOK, goodResult)), "", nil)
			if (tc.reason == "") != ok(reasons) || (tc.reason != "" && !contains(reasons, tc.reason)) {
				t.Fatalf("reasons %v inspection %+v, want reason %q", reasons, in, tc.reason)
			}
			mustNotLeak(t, in, reasons)
		})
	}
	in, _ := inspectStream([]byte(stream(validInit, assistantOK, goodResult)), "", nil)
	if !in.InitVersionExact || in.Version != "2.1.240" || in.InitAPIKeySource != "none" || in.InitPermissionMode != "default" || !in.InitInventoryPresent || in.AgentsPresent || in.AgentCount != 0 {
		t.Fatalf("init facts wrong: %+v", in)
	}
	// A non-string version is not surfaced and still fails the pinned check.
	in, reasons := inspectStream([]byte(stream(strings.Replace(validInit, `"claude_code_version":"2.1.240"`, `"claude_code_version":7`, 1), assistantOK, goodResult)), "", nil)
	if in.Version != "" || in.InitVersionExact || !contains(reasons, "version-mismatch") {
		t.Fatalf("non-string version surfaced: %+v %v", in, reasons)
	}
	in, _ = inspectStream([]byte(stream(strings.Replace(validInit, "2.1.240", "2.1.241", 1), assistantOK, goodResult)), "", nil)
	if in.Version != "2.1.241" {
		t.Fatalf("raw version not surfaced: %+v", in)
	}
	for _, tc := range []struct{ src, want string }{
		{`"ANTHROPIC_API_KEY"`, "env"}, {`"apiKeyHelper"`, "helper"}, {`"/login managed key"`, "managed"}, {`"oauth"`, "legacy"}, {`"user"`, "legacy"}, {`"SENSITIVE"`, "other"}, {`7`, "other"},
	} {
		in, _ := inspectStream([]byte(stream(strings.Replace(validInit, `"none"`, tc.src, 1), assistantOK, goodResult)), "", nil)
		if in.InitAPIKeySource != tc.want {
			t.Fatalf("%s -> %q want %q", tc.src, in.InitAPIKeySource, tc.want)
		}
	}
	in, _ = inspectStream([]byte(stream(strings.Replace(validInit, `"tools":[]`, `"tools":[],"agents":["Explore","SENSITIVE"]`, 1), assistantOK, goodResult)), "", nil)
	if !in.AgentsPresent || in.AgentCount != 2 || in.AgentsAllDocumentedBuiltin {
		t.Fatalf("agent facts wrong: %+v", in)
	}
	in, reasons = inspectStream([]byte(stream(strings.Replace(validInit, `"tools":[]`, `"tools":[],"agents":["claude","statusline-setup","claude-code-guide"]`, 1), assistantOK, goodResult)), "", nil)
	if !ok(reasons) || !in.AgentsAllDocumentedBuiltin || in.AgentCount != 3 {
		t.Fatalf("documented built-ins rejected: %+v %v", in, reasons)
	}
	in, _ = inspectStream([]byte(stream(strings.Replace(validInit, `"skills":[]`, `"skills":["a","b"]`, 1), assistantOK, goodResult)), "", nil)
	if in.SkillCount != 2 {
		t.Fatalf("skill count not recorded: %+v", in)
	}
	// Version is surfaced to the caller, so only a plain bounded dotted-numeric
	// literal is retained; anything else stays empty and still mismatches.
	for _, tc := range []struct{ name, version string }{
		{"control_char", `2.1.240\u0007`},
		{"overlong", strings.Repeat("9", 25) + "." + strings.Repeat("8", 25) + "." + strings.Repeat("7", 25)},
		{"not_dotted", "SENSITIVE"},
		{"suffixed", "2.1.240-SENSITIVE"},
	} {
		t.Run("version_"+tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(stream(strings.Replace(validInit, `"claude_code_version":"2.1.240"`, `"claude_code_version":"`+tc.version+`"`, 1), assistantOK, goodResult)), "", nil)
			if in.Version != "" || !contains(reasons, "version-mismatch") {
				t.Fatalf("hostile version surfaced: %q %v", in.Version, reasons)
			}
			mustNotLeak(t, in, reasons)
		})
	}
	// The init model is evidence the caller cannot re-derive, so a model that is
	// absent or not opus fails admission here.
	for _, tc := range []struct{ name, init string }{
		{"non_opus", strings.Replace(validInit, `"model":"claude-opus-4-8"`, `"model":"claude-sonnet-4-8"`, 1)},
		{"missing", strings.Replace(validInit, `"model":"claude-opus-4-8",`, ``, 1)},
		{"empty", strings.Replace(validInit, `"model":"claude-opus-4-8"`, `"model":""`, 1)},
	} {
		t.Run("init_model_"+tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(stream(tc.init, assistantOK, goodResult)), "", nil)
			if !contains(reasons, "init-model-invalid") || in.InitModelIsOpus {
				t.Fatalf("non-opus init model admitted: %+v %v", in, reasons)
			}
			mustNotLeak(t, in, reasons)
		})
	}
}

func TestClaudeRule1UserEchoRequiresByteExactPrompt(t *testing.T) {
	stdin := "Return the integers from 1 through 200, one per line.\n"
	echo := func(content, extra string) string {
		return `{"type":"user","parent_tool_use_id":null,"session_id":"synthetic-session","uuid":"synthetic-uuid"` + extra + `,"message":{"role":"user","content":` + content + `}}`
	}
	quoted, _ := json.Marshal(stdin)
	q := string(quoted)
	sameLen, _ := json.Marshal(strings.Repeat("x", len(stdin)))
	for _, tc := range []struct {
		name  string
		input string
		stdin string
		equal bool
	}{
		{"string_equal", stream(validInit, echo(q, ""), assistantOK, goodResult), stdin, true},
		{"block_equal", stream(validInit, echo(`[{"type":"text","text":`+q+`}]`, ""), assistantOK, goodResult), stdin, true},
		{"no_trailing_newline", stream(validInit, echo(strings.TrimSuffix(q, `\n"`)+`"`, ""), assistantOK, goodResult), stdin, false},
		{"same_length_different_bytes", stream(validInit, echo(string(sameLen), ""), assistantOK, goodResult), stdin, false},
		{"after_assistant", stream(validInit, assistantOK, echo(q, ""), goodResult), stdin, false},
		{"second_user_event", stream(validInit, echo(q, ""), echo(q, ""), assistantOK, goodResult), stdin, false},
		{"isReplay", stream(validInit, echo(q, `,"isReplay":true`), assistantOK, goodResult), stdin, false},
		{"isSynthetic_false", stream(validInit, echo(q, `,"isSynthetic":false`), assistantOK, goodResult), stdin, false},
		{"tool_use_result", stream(validInit, echo(q, `,"tool_use_result":{}`), assistantOK, goodResult), stdin, false},
		{"origin", stream(validInit, echo(q, `,"origin":"x"`), assistantOK, goodResult), stdin, false},
		{"priority", stream(validInit, echo(q, `,"priority":"now"`), assistantOK, goodResult), stdin, false},
		{"shouldQuery", stream(validInit, echo(q, `,"shouldQuery":true`), assistantOK, goodResult), stdin, false},
		{"file_attachments", stream(validInit, echo(q, `,"file_attachments":[]`), assistantOK, goodResult), stdin, false},
		{"parent_missing", stream(validInit, strings.Replace(echo(q, ""), `"parent_tool_use_id":null,`, ``, 1), assistantOK, goodResult), stdin, false},
		{"parent_non_null", stream(validInit, strings.Replace(echo(q, ""), `"parent_tool_use_id":null`, `"parent_tool_use_id":"SENSITIVE"`, 1), assistantOK, goodResult), stdin, false},
		{"role_assistant", stream(validInit, strings.Replace(echo(q, ""), `"role":"user"`, `"role":"assistant"`, 1), assistantOK, goodResult), stdin, false},
		{"two_blocks", stream(validInit, echo(`[{"type":"text","text":`+q+`},{"type":"text","text":""}]`, ""), assistantOK, goodResult), stdin, false},
		{"image_block", stream(validInit, echo(`[{"type":"image","source":{}}]`, ""), assistantOK, goodResult), stdin, false},
		{"tool_result_block", stream(validInit, echo(`[{"type":"tool_result","content":"SENSITIVE"}]`, ""), assistantOK, goodResult), stdin, false},
		{"no_stdin_known", stream(validInit, echo(q, ""), assistantOK, goodResult), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(tc.input), tc.stdin, nil)
			if ok(reasons) != tc.equal || in.PromptEqual != tc.equal || (!tc.equal && !contains(reasons, "user-event-invalid")) {
				t.Fatalf("reasons %v inspection %+v", reasons, in)
			}
			if in.UserEvents < 1 {
				t.Fatalf("user events not counted: %+v", in)
			}
			mustNotLeak(t, in, reasons, "Return the integers", "xxxx")
		})
	}
	// Rule 1 fires only on a user record: a stream without one is admissible
	// even when the prompt is unknown.
	if _, reasons := inspectStream([]byte(stream(validInit, assistantOK, goodResult)), "", nil); !ok(reasons) {
		t.Fatalf("absent user record rejected: %v", reasons)
	}
}

func TestClaudeRule2TransientAPIRetry(t *testing.T) {
	retry := func(errLit string, attempt int) string {
		return sysEvent("api_retry", `,"attempt":`+strconv.Itoa(attempt)+`,"max_retries":10,"retry_delay_ms":500,"error_status":529,"error":"`+errLit+`"`)
	}
	for _, tc := range []struct {
		name, input, reason string
		count, max          int
	}{
		{"one_overloaded", stream(validInit, retry("overloaded", 1), assistantOK, goodResult), "", 1, 1},
		{"two_transient", stream(validInit, retry("server_error", 1), retry("rate_limit", 2), assistantOK, goodResult), "", 2, 2},
		{"null_status", stream(validInit, strings.Replace(retry("overloaded", 1), `"error_status":529`, `"error_status":null`, 1), assistantOK, goodResult), "", 1, 1},
		{"three_notices", stream(validInit, retry("overloaded", 1), retry("overloaded", 1), retry("overloaded", 1), assistantOK, goodResult), "api-retry-excess", 3, 1},
		{"attempt_three", stream(validInit, retry("overloaded", 3), assistantOK, goodResult), "api-retry-excess", 1, 3},
		{"auth", stream(validInit, retry("authentication_failed", 1), assistantOK, goodResult), "api-retry-authentication_failed", 1, 1},
		{"org", stream(validInit, retry("oauth_org_not_allowed", 1), assistantOK, goodResult), "api-retry-oauth_org_not_allowed", 1, 1},
		{"hold", stream(validInit, retry("account_on_hold", 1), assistantOK, goodResult), "api-retry-account_on_hold", 1, 1},
		{"billing", stream(validInit, retry("billing_error", 1), assistantOK, goodResult), "api-retry-billing_error", 1, 1},
		{"invalid_request", stream(validInit, retry("invalid_request", 1), assistantOK, goodResult), "api-retry-invalid_request", 1, 1},
		{"model_not_found", stream(validInit, retry("model_not_found", 1), assistantOK, goodResult), "api-retry-model_not_found", 1, 1},
		{"max_output", stream(validInit, retry("max_output_tokens", 1), assistantOK, goodResult), "api-retry-max_output_tokens", 1, 1},
		{"unknown_literal", stream(validInit, retry("unknown", 1), assistantOK, goodResult), "api-retry-unknown", 1, 1},
		{"unpinned_literal", stream(validInit, retry("cloud_credential_error", 1), assistantOK, goodResult), "api-retry-other", 1, 1},
		{"hostile_literal", stream(validInit, retry("SENSITIVE", 1), assistantOK, goodResult), "api-retry-other", 1, 1},
		{"no_response_object", stream(validInit, strings.Replace(retry("overloaded", 1), `"error":"overloaded"`, `"error":{"no_response":true}`, 1), assistantOK, goodResult), "api-retry-invalid", 1, 1},
		{"attempt_string", stream(validInit, strings.Replace(retry("overloaded", 1), `"attempt":1`, `"attempt":"1"`, 1), assistantOK, goodResult), "api-retry-invalid", 1, 0},
		{"missing_uuid", stream(validInit, strings.Replace(retry("overloaded", 1), `"uuid":"synthetic-uuid",`, ``, 1), assistantOK, goodResult), "api-retry-invalid", 1, 1},
		{"delay_negative", stream(validInit, strings.Replace(retry("overloaded", 1), `"retry_delay_ms":500`, `"retry_delay_ms":-1`, 1), assistantOK, goodResult), "api-retry-invalid", 1, 1},
		{"empty_uuid", stream(validInit, strings.Replace(retry("overloaded", 1), `"uuid":"synthetic-uuid"`, `"uuid":""`, 1), assistantOK, goodResult), "api-retry-invalid", 1, 1},
		{"empty_session", stream(validInit, strings.Replace(retry("overloaded", 1), `"session_id":"synthetic-session"`, `"session_id":""`, 1), assistantOK, goodResult), "api-retry-invalid", 1, 1},
		{"no_terminal", stream(validInit, retry("overloaded", 1), assistantOK), "terminal-invalid", 1, 1},
		{"quota_rejected_same_run", stream(validInit, retry("rate_limit", 1), `{"type":"rate_limit_event","uuid":"u","session_id":"synthetic-session","rate_limit_info":{"status":"rejected"}}`, assistantOK, goodResult), "quota-rejected", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(tc.input), "", nil)
			if (tc.reason == "") != ok(reasons) || (tc.reason != "" && !contains(reasons, tc.reason)) || in.APIRetryCount != tc.count || in.APIRetryMaxAttempt != tc.max {
				t.Fatalf("reasons %v inspection %+v, want reason %q count %d max %d", reasons, in, tc.reason, tc.count, tc.max)
			}
			mustNotLeak(t, in, reasons, "cloud_credential_error")
		})
	}
	in, _ := inspectStream([]byte(stream(validInit, retry("server_error", 1), retry("rate_limit", 2), assistantOK, goodResult)), "", nil)
	if strings.Join(in.APIRetryErrors, ",") != "server_error,rate_limit" {
		t.Fatalf("retry literals not recorded: %+v", in)
	}
}

func TestClaudeRule3OverageFlagsDecide(t *testing.T) {
	event := func(info string) string {
		return `{"type":"rate_limit_event","uuid":"u","session_id":"synthetic-session","rate_limit_info":` + info + `}`
	}
	for _, tc := range []struct {
		info, reason string
		attested     bool
	}{
		{`{"status":"allowed","rateLimitType":"seven_day_overage_included","isUsingOverage":false,"overageInUse":false}`, "", true},
		{`{"status":"allowed_warning","rateLimitType":"five_hour","isUsingOverage":false,"overageInUse":false,"utilization":0.5,"surpassedThreshold":0.5,"overageDisabledReason":"org_level_disabled","canUserPurchaseCredits":false,"hasChargeableSavedPaymentMethod":false}`, "", true},
		{`{"status":"allowed"}`, "", false},
		{`{"status":"allowed","isUsingOverage":false}`, "", false},
		{`{"status":"allowed","isUsingOverage":true,"overageInUse":false}`, "overage-in-use", false},
		{`{"status":"allowed","isUsingOverage":false,"overageInUse":true}`, "overage-in-use", false},
		{`{"status":"allowed","rateLimitType":"overage"}`, "overage-in-use", false},
		{`{"status":"allowed","errorCode":"credits_required"}`, "credits-required", false},
		{`{"status":"allowed","errorCode":"SENSITIVE"}`, "rate-limit-invalid", false},
		{`{"status":"allowed","isUsingOverage":"false"}`, "rate-limit-invalid", false},
		{`{"status":"allowed","rateLimitType":"SENSITIVE"}`, "rate-limit-invalid", false},
		{`{"status":"allowed","overageDisabledReason":7}`, "rate-limit-invalid", false},
		{`{"status":"allowed","overageStatus":"allowed_warning"}`, "overage-not-rejected", false},
	} {
		in, reasons := inspectStream([]byte(stream(validInit, event(tc.info), assistantOK, goodResult)), "", nil)
		if (tc.reason == "") != ok(reasons) || (tc.reason != "" && !contains(reasons, tc.reason)) || in.OverageAttested != tc.attested || in.RateLimitEvents != 1 {
			t.Fatalf("%s: reasons %v inspection %+v", tc.info, reasons, in)
		}
		mustNotLeak(t, in, reasons)
	}
	in, _ := inspectStream([]byte(stream(validInit, event(`{"status":"allowed","rateLimitType":"seven_day_overage_included"}`), event(`{"status":"allowed","rateLimitType":"five_hour"}`), assistantOK, goodResult)), "", nil)
	if strings.Join(in.RateLimitTypes, ",") != "seven_day_overage_included,five_hour" {
		t.Fatalf("types not recorded: %+v", in)
	}
	in, _ = inspectStream([]byte(stream(validInit, event(`{"status":"allowed","isUsingOverage":false,"overageInUse":false}`), event(`{"status":"allowed"}`), assistantOK, goodResult)), "", nil)
	if in.OverageAttested {
		t.Fatal("one event without flags must not attest the run")
	}
}

func TestClaudeRule5PermittedTailAfterResult(t *testing.T) {
	idle := sysEvent("session_state_changed", `,"state":"idle"`)
	statusNull := sysEvent("status", `,"status":null`)
	thinking := sysEvent("thinking_tokens", `,"estimated_tokens":12,"estimated_tokens_delta":3`)
	for _, tc := range []struct {
		name, input, reason string
		tail                int
	}{
		{"idle_only", stream(validInit, assistantOK, goodResult, idle), "", 1},
		{"all_three", stream(validInit, assistantOK, goodResult, idle, statusNull, thinking), "", 3},
		{"idle_twice", stream(validInit, assistantOK, goodResult, idle, idle), "tail-invalid", 1},
		{"running_after", stream(validInit, assistantOK, goodResult, sysEvent("session_state_changed", `,"state":"running"`)), "tail-invalid", 0},
		{"requesting_after", stream(validInit, assistantOK, goodResult, sysEvent("status", `,"status":"requesting"`)), "tail-invalid", 0},
		{"status_with_permission_after", stream(validInit, assistantOK, goodResult, sysEvent("status", `,"status":null,"permissionMode":"default"`)), "tail-invalid", 0},
		{"second_result", stream(validInit, assistantOK, goodResult, goodResult), "tail-invalid", 0},
		{"assistant_after", stream(validInit, assistantOK, goodResult, assistantOK), "tail-invalid", 0},
		{"user_after", stream(validInit, assistantOK, goodResult, `{"type":"user","parent_tool_use_id":null,"message":{"role":"user","content":"x"}}`), "tail-invalid", 0},
		{"thinking_negative", stream(validInit, assistantOK, goodResult, sysEvent("thinking_tokens", `,"estimated_tokens":-1,"estimated_tokens_delta":0`)), "tail-invalid", 0},
		{"unknown_after", stream(validInit, assistantOK, goodResult, sysEvent("notification", `,"payload":"SENSITIVE"`)), "tail-invalid", 0},
		{"running_before", stream(validInit, sysEvent("session_state_changed", `,"state":"running"`), assistantOK, goodResult), "", 0},
		{"requires_action_before", stream(validInit, sysEvent("session_state_changed", `,"state":"requires_action"`), assistantOK, goodResult), "session-state-invalid", 0},
		{"thinking_before", stream(validInit, thinking, assistantOK, goodResult), "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(tc.input), "", nil)
			if (tc.reason == "") != ok(reasons) || (tc.reason != "" && !contains(reasons, tc.reason)) || in.TailEvents != tc.tail {
				t.Fatalf("reasons %v inspection %+v, want reason %q tail %d", reasons, in, tc.reason, tc.tail)
			}
			mustNotLeak(t, in, reasons)
		})
	}
}

func TestClaudeRule6StatusRequestingOrNullOnly(t *testing.T) {
	for _, tc := range []struct{ extra, reason string }{
		{`,"status":null`, ""},
		{`,"status":"requesting"`, ""},
		{`,"status":"requesting","permissionMode":"default"`, ""},
		{`,"status":"compacting"`, "compaction-activity"},
		{`,"status":null,"compact_result":"success"`, "compaction-activity"},
		{`,"status":null,"compact_error":"SENSITIVE"`, "compaction-activity"},
		{`,"status":"requesting","permissionMode":"bypassPermissions"`, "permission-mode-changed"},
		{`,"status":"SENSITIVE"`, "unknown-event"},
		{``, "unknown-event"},
	} {
		in, reasons := inspectStream([]byte(stream(validInit, sysEvent("status", tc.extra), assistantOK, goodResult)), "", nil)
		if (tc.reason == "") != ok(reasons) || (tc.reason != "" && !contains(reasons, tc.reason)) {
			t.Fatalf("%s: reasons %v inspection %+v", tc.extra, reasons, in)
		}
		if tc.reason == "" && in.StatusEvents != 1 {
			t.Fatalf("%s: status not counted: %+v", tc.extra, in)
		}
		mustNotLeak(t, in, reasons)
	}
}

func TestClaudeRule7AgentsWithSubagentActivityFail(t *testing.T) {
	init := strings.Replace(validInit, `"tools":[]`, `"tools":[],"agents":["Explore"]`, 1)
	for _, tc := range []struct{ name, record, reason string }{
		{"non_null_parent", strings.Replace(assistantOK, `"parent_tool_use_id":null`, `"parent_tool_use_id":"SENSITIVE"`, 1), "subagent-activity"},
		{"tool_progress", `{"type":"tool_progress","tool_use_id":"SENSITIVE","tool_name":"Agent","parent_tool_use_id":null,"elapsed_time_seconds":1,"uuid":"u","session_id":"synthetic-session"}`, "tool-activity"},
		{"task_started", sysEvent("task_started", `,"task_id":"SENSITIVE"`), "task-activity"},
		{"task_notification", sysEvent("task_notification", `,"task_id":"SENSITIVE"`), "task-activity"},
		{"background_tasks", sysEvent("background_tasks_changed", `,"tasks":[]`), "task-activity"},
		{"memory_recall", sysEvent("memory_recall", `,"content":"SENSITIVE"`), "memory-activity"},
		{"elicitation", sysEvent("elicitation_complete", ``), "elicitation-activity"},
		{"fallback", sysEvent("model_refusal_fallback", `,"model":"SENSITIVE"`), "fallback-activity"},
		{"no_fallback", sysEvent("model_refusal_no_fallback", ``), "fallback-activity"},
		{"compact_boundary", sysEvent("compact_boundary", `,"compact_metadata":{}`), "compaction-activity"},
		{"plugin_install", sysEvent("plugin_install", `,"plugin":"SENSITIVE"`), "startup-activity"},
		{"assistant_error", strings.Replace(assistantOK, `"parent_tool_use_id":null`, `"parent_tool_use_id":null,"error":"authentication_failed"`, 1), "assistant-error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(stream(init, tc.record, assistantOK, goodResult)), "", nil)
			if ok(reasons) || !contains(reasons, tc.reason) {
				t.Fatalf("reasons %v inspection %+v, want %q", reasons, in, tc.reason)
			}
			mustNotLeak(t, in, reasons, "authentication_failed")
		})
	}
	in, _ := inspectStream([]byte(stream(init, strings.Replace(assistantOK, `"parent_tool_use_id":null`, `"parent_tool_use_id":"x"`, 1), assistantOK, goodResult)), "", nil)
	if in.NonNullParentRecords != 1 {
		t.Fatalf("non-null parents not counted: %+v", in)
	}
}

func TestClaudeItem2TerminalAndUsageMetadata(t *testing.T) {
	opus := `{"claude-opus-4-8":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":2,"cacheCreationInputTokens":1,"webSearchRequests":0,"provider":"firstParty"}}`
	for _, tc := range []struct{ name, result, reason string }{
		{"complete", withUsage(opus), ""},
		{"num_turns_missing", strings.Replace(goodResult, `"num_turns":1,`, ``, 1), "terminal-invalid"},
		{"num_turns_negative", strings.Replace(goodResult, `"num_turns":1`, `"num_turns":-1`, 1), "terminal-invalid"},
		{"num_turns_fraction", strings.Replace(goodResult, `"num_turns":1`, `"num_turns":1.5`, 1), "terminal-invalid"},
		{"num_turns_huge", strings.Replace(goodResult, `"num_turns":1`, `"num_turns":100000000000000000`, 1), "terminal-invalid"},
		// D4 tightens the harness here: only end_turn admits, max_tokens is
		// answer-truncated and everything else is stop-reason-invalid.
		{"stop_null", strings.Replace(goodResult, `"stop_reason":"end_turn"`, `"stop_reason":null`, 1), "stop-reason-invalid"},
		{"stop_other", strings.Replace(goodResult, `"stop_reason":"end_turn"`, `"stop_reason":"SENSITIVE"`, 1), "stop-reason-invalid"},
		{"stop_missing", strings.Replace(goodResult, `"stop_reason":"end_turn",`, ``, 1), "terminal-invalid"},
		{"denials_missing", strings.Replace(goodResult, `"permission_denials":[],`, ``, 1), "terminal-invalid"},
		{"denial", strings.Replace(goodResult, `"permission_denials":[]`, `"permission_denials":[{"tool_name":"SENSITIVE"}]`, 1), "denial-activity"},
		{"deferred", strings.Replace(goodResult, `"result":"OK"`, `"result":"OK","deferred_tool_use":{}`, 1), "tool-activity"},
		{"web_search", withUsage(strings.Replace(opus, `"webSearchRequests":0`, `"webSearchRequests":1`, 1)), "web-search-activity"},
		{"provider_other", withUsage(strings.Replace(opus, `"firstParty"`, `"bedrock"`, 1)), "route-invalid"},
		{"provider_missing", withUsage(strings.Replace(opus, `,"provider":"firstParty"`, ``, 1)), "route-invalid"},
		{"tokens_string", withUsage(strings.Replace(opus, `"inputTokens":10`, `"inputTokens":"10"`, 1)), "usage-invalid"},
		{"tokens_negative", withUsage(strings.Replace(opus, `"outputTokens":5`, `"outputTokens":-5`, 1)), "usage-invalid"},
		{"entry_not_object", withUsage(`{"claude-opus-4-8":7}`), "usage-invalid"},
		{"empty_entry", withUsage(`{"claude-opus-4-8":{}}`), "route-invalid"},
		{"session_missing", strings.Replace(goodResult, `"session_id":"synthetic-session",`, ``, 1), "session-inconsistent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(stream(validInit, assistantOK, tc.result)), "", nil)
			if (tc.reason == "") != ok(reasons) || (tc.reason != "" && !contains(reasons, tc.reason)) {
				t.Fatalf("reasons %v inspection %+v, want %q", reasons, in, tc.reason)
			}
			mustNotLeak(t, in, reasons, "bedrock")
		})
	}
	in, _ := inspectStream([]byte(stream(validInit, assistantOK, withUsage(opus))), "", nil)
	if !in.UsageComplete || !in.WebSearchZero || in.UsageProvider != "firstParty" || in.OpusInputTokens != 10 || in.OpusOutputTokens != 5 || in.OpusCacheTokens != 3 || in.NonOpusInputTokens != 0 || in.StopReasonKind != "end_turn" || in.NumTurns != 1 || in.DenialCount != 0 {
		t.Fatalf("usage facts wrong: %+v", in)
	}
	mixed := `{"claude-opus-4-8":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"webSearchRequests":0,"provider":"firstParty"},"claude-haiku-4-5":{"inputTokens":7,"outputTokens":3,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"webSearchRequests":0}}`
	in, reasons := inspectStream([]byte(stream(validInit, assistantOK, withUsage(mixed))), "", nil)
	if !contains(reasons, "usage-model-invalid") || in.Answer != "" || in.NonOpusInputTokens != 7 || in.NonOpusOutputTokens != 3 || in.UsageProvider != "mixed" || in.UsageOpusModels != 1 || in.UsageModels != 2 {
		t.Fatalf("mixed usage facts wrong: %+v %v", in, reasons)
	}
	in, _ = inspectStream([]byte(stream(validInit, assistantOK, withUsage(`{"claude-opus-4-8":{}}`))), "", nil)
	if in.UsageComplete || in.WebSearchZero || in.UsageProvider != "missing" || in.OpusInputTokens != 0 {
		t.Fatalf("unknown metadata fabricated: %+v", in)
	}
	in, _ = inspectStream([]byte(stream(validInit, assistantOK, strings.Replace(goodResult, `"stop_reason":"end_turn"`, `"stop_reason":null`, 1))), "", nil)
	if in.StopReasonKind != "null" {
		t.Fatalf("null stop reason not recorded: %+v", in)
	}
}

func TestClaudeItem11DecisionBooleans(t *testing.T) {
	in, reasons := inspectStream([]byte(stream(validInit, assistantOK, goodResult)), "", []string{"/other", "/private/synthetic"})
	if !ok(reasons) || !in.SessionConsistent || !in.CwdMatches {
		t.Fatalf("booleans wrong on valid stream: %+v %v", in, reasons)
	}
	in, reasons = inspectStream([]byte(stream(validInit, assistantOK, goodResult)), "", []string{"/elsewhere"})
	if ok(reasons) || in.CwdMatches || !contains(reasons, "cwd-mismatch") {
		t.Fatalf("cwd mismatch admitted: %+v %v", in, reasons)
	}
	in, reasons = inspectStream([]byte(stream(strings.Replace(validInit, `"cwd":"/private/synthetic",`, ``, 1), assistantOK, goodResult)), "", []string{"/private/synthetic"})
	if ok(reasons) || in.CwdMatches || !contains(reasons, "cwd-mismatch") {
		t.Fatalf("missing cwd admitted: %+v %v", in, reasons)
	}
	// A child of an accepted root is not the root.
	in, reasons = inspectStream([]byte(stream(strings.Replace(validInit, `"cwd":"/private/synthetic"`, `"cwd":"/private/synthetic/x"`, 1), assistantOK, goodResult)), "", []string{"/private/synthetic"})
	if ok(reasons) || in.CwdMatches || !contains(reasons, "cwd-mismatch") {
		t.Fatalf("cwd prefix admitted: %+v %v", in, reasons)
	}
	in, reasons = inspectStream([]byte(stream(validInit, assistantOK, goodResult)), "", []string{"/private/synthetic"})
	if !ok(reasons) || !in.CwdMatches {
		t.Fatalf("exact cwd rejected: %+v %v", in, reasons)
	}
	in, reasons = inspectStream([]byte(stream(validInit, strings.Replace(assistantOK, "synthetic-session", "SENSITIVE-other", 1), goodResult)), "", nil)
	if ok(reasons) || in.SessionConsistent || !contains(reasons, "session-inconsistent") {
		t.Fatalf("session drift admitted: %+v %v", in, reasons)
	}
	mustNotLeak(t, in, reasons)
	in, reasons = inspectStream([]byte(stream(strings.Replace(validInit, `"session_id":"synthetic-session"`, `"session_id":""`, 1), assistantOK, goodResult)), "", nil)
	if ok(reasons) || in.SessionConsistent {
		t.Fatalf("empty init session admitted: %+v %v", in, reasons)
	}
}

func TestClaudePreservesIndependentFailures(t *testing.T) {
	input := strings.Replace(validInit, `"plugins":[]`, `"plugins":[{"name":"SENSITIVE"}]`, 1) + `{"type":"SENSITIVE","session_id":"synthetic-session"}` + goodResult
	in, reasons := inspectStream([]byte(input), "", nil)
	if strings.Join(reasons, ",") != "inventory-invalid,unknown-event,response-model-invalid" || in.UnknownEvents != 1 || strings.Join(in.UnknownKinds, ",") != "top:other" {
		t.Fatalf("lost independent evidence: %+v %v", in, reasons)
	}
	mustNotLeak(t, in, reasons)
}

func TestClaudeSeparatesResponseFromUsageModels(t *testing.T) {
	for _, model := range []string{"claude-opus-4-8", "claude-sonnet-5", ""} {
		input := validInit + strings.Replace(assistantOK, "claude-opus-4-8", model, 1) + withUsage(`{"claude-opus-4-8":{},"claude-haiku-4-5":{}}`)
		in, _ := inspectStream([]byte(input), "", nil)
		wantOpus := 0
		if model == "claude-opus-4-8" {
			wantOpus = 1
		}
		if !in.InitModelPresent || !in.InitModelIsOpus || in.ResponseModels != 1 || in.ResponseOpusModels != wantOpus || in.UsageModels != 2 || in.UsageOpusModels != 1 {
			t.Fatalf("conflated model sources: %+v", in)
		}
	}
}

func TestClaudeClassifiesDocumentedRateLimitEnvelope(t *testing.T) {
	for _, tc := range []struct {
		info    string
		success bool
	}{
		{`{"status":"allowed"}`, true},
		{`{"status":"allowed_warning","resetsAt":123,"utilization":0.9}`, true},
		{`{"status":"rejected"}`, false},
		{`{"status":"future"}`, false},
		{`{"status":false}`, false},
		{`{"status":"allowed","overageStatus":"allowed"}`, false},
		{`{"status":"allowed","overageStatus":"rejected"}`, true},
		{`{"status":"allowed","utilization":"SENSITIVE"}`, false},
	} {
		input := validInit + `{"type":"rate_limit_event","uuid":"synthetic","session_id":"synthetic-session","rate_limit_info":` + tc.info + `}` + assistantOK + goodResult
		in, reasons := inspectStream([]byte(input), "", nil)
		if ok(reasons) != tc.success || in.RateLimitEvents != 1 || in.UnknownEvents != 0 {
			t.Fatalf("rate verdict: %+v %v", in, reasons)
		}
	}
	if _, reasons := inspectStream([]byte(validInit+`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`+assistantOK+goodResult), "", nil); ok(reasons) {
		t.Fatal("missing envelope identity accepted")
	}
	// An explicitly empty identity is as absent as a missing one.
	if _, reasons := inspectStream([]byte(validInit+`{"type":"rate_limit_event","uuid":"","session_id":"synthetic-session","rate_limit_info":{"status":"allowed"}}`+assistantOK+goodResult), "", nil); ok(reasons) {
		t.Fatal("empty envelope identity accepted")
	}
}

func TestClaudeResultChecksReplyAndResolvedModel(t *testing.T) {
	for _, tc := range []struct {
		result          string
		ok, model, opus bool
	}{
		{withUsage(`{"claude-opus-4-8":{}}`), true, true, true},
		{strings.Replace(withUsage(`{"claude-sonnet-5":{}}`), `"result":"OK"`, `"result":"not OK"`, 1), false, true, false},
		{goodResult, true, false, false},
	} {
		in, _ := inspectStream([]byte(stream(validInit, assistantOK, tc.result)), "", nil)
		if in.ReplyOK != tc.ok || in.ModelPresent != tc.model || in.ModelIsOpus != tc.opus {
			t.Fatalf("reply/model predicate wrong: %+v", in)
		}
	}
}

func TestClaudeProtocolAcceptsOnlyExplicitSuccess(t *testing.T) {
	marker := `{\"type\":\"tool_use\"}`
	for _, tc := range []struct {
		name, input string
		success     bool
	}{
		{"stream", stream(validInit, assistantOK, goodResult), true},
		{"text_and_thinking", validInit +
			strings.Replace(assistantOK, `[{"type":"text","text":"OK"}]`, `[{"type":"text","text":"tool_use hook_started"},{"type":"thinking","thinking":"reason"},{"type":"redacted_thinking","data":"opaque"}]`, 1) +
			strings.Replace(goodResult, `"result":"OK"`, `"result":"tool_use hook_started"`, 1), true},
		{"quoted_marker", validInit +
			strings.Replace(assistantOK, `"text":"OK"`, `"text":"`+marker+`"`, 1) +
			strings.Replace(goodResult, `"result":"OK"`, `"result":"`+marker+`"`, 1), true},
		{"missing_terminal", validInit, false},
		{"missing_error_flag", validInit + assistantOK + strings.Replace(goodResult, `"is_error":false,`, ``, 1), false},
		{"false_as_string", validInit + assistantOK + strings.Replace(goodResult, `"is_error":false`, `"is_error":"false"`, 1), false},
		{"error", validInit + assistantOK + `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["SENSITIVE"],"num_turns":1,"stop_reason":null,"permission_denials":[],"session_id":"synthetic-session","uuid":"u"}`, false},
		{"empty", validInit + assistantOK + strings.Replace(goodResult, `"result":"OK"`, `"result":" "`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(tc.input), "", nil)
			if ok(reasons) != tc.success {
				t.Fatalf("reasons %v inspection %+v, want success %v", reasons, in, tc.success)
			}
			mustNotLeak(t, in, reasons)
		})
	}
}

func TestClaudeRejectsAmbiguousJSON(t *testing.T) {
	// Input that cannot be decoded at all yields the malformed literal alone.
	for _, input := range []string{
		"", goodResult + "garbage", `[]`, `null`, `{"type":`,
		goodResult + `{"unfinished":`, goodResult + `[`,
		strings.Replace(goodResult, "OK", "\xff", 1),
		strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66),
	} {
		if _, reasons := inspectStream([]byte(input), "", nil); strings.Join(reasons, ",") != "malformed" {
			t.Errorf("ambiguous input %q: reasons %v", input, reasons)
		}
	}
	// Duplicate keys inside an otherwise admissible stream: the ambiguity is
	// the only thing standing between these and a clean verdict, including
	// when the second spelling is escaped.
	for _, tc := range []struct{ name, result string }{
		{"top_level", strings.Replace(goodResult, `{"type":"result",`, `{"type":"result","type":"result",`, 1)},
		{"nested_object", strings.Replace(goodResult, `"result":"OK"`, `"result":"OK","usage":{"a":1,"a":2}`, 1)},
		{"escaped_in_array", strings.Replace(goodResult, `"result":"OK"`, `"result":"OK","usage":[{"a":1,"\u0061":2}]`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, reasons := inspectStream([]byte(stream(validInit, assistantOK, tc.result)), "", nil); strings.Join(reasons, ",") != "malformed" {
				t.Fatalf("duplicate key admitted: reasons %v", reasons)
			}
			// The same stream without the duplicate is clean, so nothing but
			// the duplicate can be producing the rejection above.
			if _, reasons := inspectStream([]byte(stream(validInit, assistantOK, goodResult)), "", nil); !ok(reasons) {
				t.Fatalf("control stream rejected: %v", reasons)
			}
		})
	}
	// Two terminals decode cleanly and are rejected on their merits, not as
	// malformed input.
	if _, reasons := inspectStream([]byte(stream(validInit, assistantOK, goodResult, goodResult)), "", nil); !contains(reasons, "tail-invalid") || contains(reasons, "malformed") {
		t.Fatalf("second terminal misclassified: %v", reasons)
	}
	// Malformed input names the record that failed to decode, with no content.
	in, reasons := inspectStream([]byte(validInit+"\n"+`{"type":`), "", nil)
	if strings.Join(reasons, ",") != "malformed" || in.DecodeErrorIndex != 1 {
		t.Fatalf("decode error index wrong: %+v %v", in, reasons)
	}
	if in, _ := inspectStream([]byte(stream(validInit, assistantOK, goodResult)), "", nil); in.DecodeErrorIndex != -1 {
		t.Fatalf("decode error index must be -1 on clean input: %+v", in)
	}
}

func TestClaudeBoundsInputAndReportsActivityWithoutPayload(t *testing.T) {
	for _, input := range []string{strings.Repeat(" ", 1<<20) + goodResult, strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66)} {
		if _, reasons := inspectStream([]byte(input), "", nil); strings.Join(reasons, ",") != "malformed" {
			t.Fatalf("unbounded input accepted: %v", reasons)
		}
	}
	in, reasons := inspectStream([]byte(validInit+`{"type":"system","subtype":"hook_response","output":"SENSITIVE"}{"type":"assistant","message":{"content":[{"type":"tool_use","input":"SENSITIVE"}]}}`+goodResult), "", nil)
	if ok(reasons) || in.ToolEvents != 1 || in.StartupEvents != 1 || in.TerminalCount != 1 {
		t.Fatalf("missing activity evidence: %+v %v", in, reasons)
	}
	mustNotLeak(t, in, reasons)
}

func TestClaudeStreamRequiresEmptyInventoryAndNoActivity(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"missing_init", goodResult},
		{"duplicate_init", validInit + validInit + goodResult},
		{"missing_plugins", `{"type":"system","subtype":"init","tools":[],"mcp_servers":[]}` + goodResult},
		{"null_inventory", `{"type":"system","subtype":"init","tools":null,"mcp_servers":[],"plugins":[]}` + goodResult},
		{"tool_inventory", `{"type":"system","subtype":"init","tools":["Bash"],"mcp_servers":[],"plugins":[]}` + goodResult},
		{"mcp_inventory", `{"type":"system","subtype":"init","tools":[],"mcp_servers":[{}],"plugins":[]}` + goodResult},
		{"plugin_inventory", `{"type":"system","subtype":"init","tools":[],"mcp_servers":[],"plugins":[{}]}` + goodResult},
		{"hook", validInit + `{"type":"system","subtype":"hook_started"}` + goodResult},
		{"tool_event", validInit + `{"type":"tool_use"}` + goodResult},
		{"assistant_tool", validInit + `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}` + goodResult},
		{"unknown_block", validInit + `{"type":"assistant","message":{"content":[{"type":"future"}]}}` + goodResult},
		{"unknown_event", validInit + `{"type":"future"}` + goodResult},
		{"duplicate_terminal", validInit + goodResult + goodResult},
		{"after_terminal", validInit + goodResult + assistantOK},
		{"bad_nested_duplicate", validInit + `{"type":"assistant","message":{"content":[],"content":[]}}` + goodResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, reasons := inspectStream([]byte(tc.input), "", nil); ok(reasons) {
				t.Fatalf("unsafe stream accepted: %s", tc.input)
			}
		})
	}
	in, _ := inspectStream([]byte(`{"type":"system","subtype":"init","tools":["Bash"],"mcp_servers":[{}],"plugins":[{}]}`+goodResult), "", nil)
	if in.InitCount != 1 || in.ToolCount != 1 || in.MCPCount != 1 || in.PluginCount != 1 {
		t.Fatalf("missing redacted inventory counts: %+v", in)
	}
}

func TestInspectStreamAnswerSemantics(t *testing.T) {
	two := `{"type":"assistant","parent_tool_use_id":null,"session_id":"synthetic-session","uuid":"u","message":{"model":"claude-opus-4-8","role":"assistant","content":[{"type":"thinking","thinking":"SENSITIVE"},{"type":"text","text":"Hello"}]}}`
	second := strings.Replace(two, `"text":"Hello"`, `"text":" world"`, 1)
	result := func(text string) string {
		return strings.Replace(goodResult, `"result":"OK"`, `"result":"`+text+`"`, 1)
	}
	// JSON-escaped C0 controls and DEL; \n and \t survive sanitizing.
	ctrl := `a\u001b[31mb\u0007c\u007f\n\td`
	ctrlText := strings.Replace(two, `"text":"Hello"`, `"text":"`+ctrl+`"`, 1)
	for _, tc := range []struct {
		name, input, reason, answer string
	}{
		{"concatenated_blocks", stream(validInit, two, second, result("Hello world")), "", "Hello world"},
		{"last_message_only", stream(validInit, two, second, result(" world")), "", " world"},
		{"trailing_whitespace_tolerated", stream(validInit, two, result(`Hello\n`)), "", "Hello\n"},
		{"mismatch", stream(validInit, two, result("Goodbye")), "answer-inconsistent", ""},
		{"max_tokens", stream(validInit, two, strings.Replace(result("Hello"), `"end_turn"`, `"max_tokens"`, 1)), "answer-truncated", ""},
		{"stop_null", stream(validInit, two, strings.Replace(result("Hello"), `"stop_reason":"end_turn"`, `"stop_reason":null`, 1)), "stop-reason-invalid", ""},
		{"terminal_reason", stream(validInit, two, strings.Replace(result("Hello"), `"result":`, `"terminal_reason":"SENSITIVE","result":`, 1)), "terminal-marker-present", ""},
		{"aborted", stream(validInit, two, strings.Replace(result("Hello"), `"result":`, `"aborted":true,"result":`, 1)), "terminal-marker-present", ""},
		{"supersedes", stream(validInit, strings.Replace(two, `"uuid":"u"`, `"uuid":"u","supersedes":"x"`, 1), result("Hello")), "terminal-marker-present", ""},
		{"control_chars", stream(validInit, ctrlText, result(ctrl)), "", "a�[31mb�c�\n\td"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(tc.input), "", nil)
			if (tc.reason == "") != (len(reasons) == 0) || (tc.reason != "" && !contains(reasons, tc.reason)) || in.Answer != tc.answer {
				t.Fatalf("reasons=%v answer=%q want %q/%q", reasons, in.Answer, tc.reason, tc.answer)
			}
			if b, _ := json.Marshal(in); strings.Contains(string(b), "SENSITIVE") {
				t.Fatal("thinking or vendor text leaked into inspection")
			}
		})
	}
}

func TestClaudeUnknownDiagnosticsUseFixedBuckets(t *testing.T) {
	input := validInit + `{"type":"user","message":"SENSITIVE"}{"type":"system","subtype":"status","payload":"SENSITIVE"}{"type":"SENSITIVE"}{"type":"assistant","message":{"content":[{"type":"SENSITIVE"}]}}` + goodResult
	in, reasons := inspectStream([]byte(input), "", nil)
	if ok(reasons) || strings.Join(in.UnknownKinds, ",") != "system:status,top:other,assistant:envelope,assistant:content" || !contains(reasons, "user-event-invalid") {
		t.Fatalf("missing fixed buckets: %+v %v", in, reasons)
	}
	mustNotLeak(t, in, reasons)
}

func TestClaudeEmptyEvidenceListsSerializeAsArrays(t *testing.T) {
	// Downstream gates test emptiness; a clean verdict must serialize [] not null.
	in, reasons := inspectStream([]byte(stream(validInit, assistantOK, goodResult)), "", nil)
	if !ok(reasons) {
		t.Fatalf("clean stream rejected: %v", reasons)
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"unknown_kinds":[]`, `"api_retry_errors":[]`, `"rate_limit_types":[]`} {
		if !strings.Contains(string(b), field) {
			t.Fatalf("empty list serialized as null, want %s: %s", field, b)
		}
	}
}

func TestUsageTotalsRejectOverflow(t *testing.T) {
	for _, fields := range [][]string{
		{"inputTokens"}, {"outputTokens"}, {"cacheReadInputTokens"},
		{"cacheCreationInputTokens"}, {"cacheReadInputTokens", "cacheCreationInputTokens"},
	} {
		t.Run(strings.Join(fields, "+"), func(t *testing.T) {
			// 1024 * 2^53 overflows int64; both cache fields share one total.
			entry := map[string]any{"provider": "firstParty"}
			for _, field := range fields {
				entry[field] = int64(1 << 53)
			}
			models := make(map[string]any)
			for i := 0; i < 1024/len(fields); i++ {
				models["claude-opus-4-8-"+strconv.Itoa(i)] = entry
			}
			raw, err := json.Marshal(models)
			if err != nil {
				t.Fatal(err)
			}
			input := []byte(stream(validInit, assistantOK, withUsage(string(raw))))
			if len(input) >= maxOutputBytes {
				t.Fatal("overflow fixture must fit the transcript limit")
			}
			in, reasons := inspectStream(input, "", nil)
			if !contains(reasons, "usage-invalid") || in.Answer != "" {
				t.Fatalf("overflow admitted: input=%d output=%d cache=%d reasons=%v", in.OpusInputTokens, in.OpusOutputTokens, in.OpusCacheTokens, reasons)
			}
			delete(models, "claude-opus-4-8-0")
			raw, err = json.Marshal(models)
			if err != nil {
				t.Fatal(err)
			}
			if _, reasons := inspectStream([]byte(stream(validInit, assistantOK, withUsage(string(raw)))), "", nil); len(reasons) != 0 {
				t.Fatalf("representable totals rejected: %v", reasons)
			}
		})
	}
}

func TestInspectStreamBoundsTheAnswer(t *testing.T) {
	// The answer crosses into agent.Advisory, which caps at MaxAnswerBytes.
	// unit is one JSON-escaped character repeated n times in both the
	// assistant text and the terminal result.
	body := func(unit string, n int) string {
		text := strings.Repeat(unit, n)
		return stream(validInit,
			strings.Replace(assistantOK, `"text":"OK"`, `"text":"`+text+`"`, 1),
			strings.Replace(goodResult, `"result":"OK"`, `"result":"`+text+`"`, 1))
	}
	// Each DEL sanitizes to a three-byte U+FFFD, so the cap must be measured
	// on the sanitized answer: 21846 DELs are 21846 raw bytes but 65538 sent.
	for _, tc := range []struct {
		name, unit string
		n, want    int
	}{
		{"plain_over", "x", 65537, -1},
		{"plain_at_cap", "x", 65536, 65536},
		{"expanding_over", `\u007f`, 21846, -1},
		{"expanding_under_cap", `\u007f`, 21845, 65535},
		{"crlf_near_raw_cap", `x\r\n`, 21845, 43690},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, reasons := inspectStream([]byte(body(tc.unit, tc.n)), "", nil)
			if tc.want < 0 {
				if !contains(reasons, "answer-too-large") || in.Answer != "" {
					t.Fatalf("oversize answer retained: %d bytes, reasons %v", len(in.Answer), reasons)
				}
				return
			}
			if !ok(reasons) || len(in.Answer) != tc.want {
				t.Fatalf("answer of %d bytes rejected, want %d: reasons %v", len(in.Answer), tc.want, reasons)
			}
		})
	}
}

func TestSanitizeNormalizesOnlyCRLF(t *testing.T) {
	if got := sanitize("one\r\ntwo\rthree\r\r\nfour\r"); got != "one\ntwo�three�\nfour�" {
		t.Fatalf("CRLF normalization or standalone CR safety changed: %q", got)
	}
}

func TestClaudeTerminalRejectsEachFailureFlagAlone(t *testing.T) {
	// Each of these is the only thing wrong with an otherwise clean terminal.
	for _, tc := range []struct{ name, result string }{
		{"is_error_alone", strings.Replace(goodResult, `"is_error":false`, `"is_error":true`, 1)},
		{"error_subtype_alone", strings.Replace(goodResult, `"subtype":"success"`, `"subtype":"error_during_execution"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, reasons := inspectStream([]byte(stream(validInit, assistantOK, tc.result)), "", nil); !contains(reasons, "terminal-invalid") {
				t.Fatalf("terminal admitted: %v", reasons)
			}
		})
	}
}

func TestClaudeBoundsRecordCountAndNestingDepth(t *testing.T) {
	// Record cap: the 4097th record is refused, 4096 decode.
	record := `{"a":1}` + "\n"
	if _, reasons := inspectStream([]byte(strings.Repeat(record, 4097)), "", nil); !contains(reasons, "malformed") {
		t.Fatalf("record cap not enforced: %v", reasons)
	}
	if _, reasons := inspectStream([]byte(strings.Repeat(record, 4096)), "", nil); contains(reasons, "malformed") {
		t.Fatalf("4096 records must decode: %v", reasons)
	}
	// Depth bound inside a record object, where the top-level object check
	// cannot stand in for it.
	nested := func(depth int) string {
		return stream(validInit, `{"type":"assistant","x":`+strings.Repeat("[", depth)+"0"+strings.Repeat("]", depth)+`}`, goodResult)
	}
	if _, reasons := inspectStream([]byte(nested(70)), "", nil); !contains(reasons, "malformed") {
		t.Fatalf("depth bound not enforced inside a record: %v", reasons)
	}
	if _, reasons := inspectStream([]byte(nested(60)), "", nil); contains(reasons, "malformed") {
		t.Fatalf("60-deep nesting must decode: %v", reasons)
	}
}
