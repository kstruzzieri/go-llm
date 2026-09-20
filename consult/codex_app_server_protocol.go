package consult

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	errAppServerProtocol    = errors.New("consult: invalid App Server protocol")
	errAppServerRequest     = errors.New("consult: unsupported App Server request")
	errAppServerVendor      = errors.New("consult: App Server error")
	errAppServerUnavailable = errors.New("consult: App Server model unavailable")
	errAppServerIncomplete  = errors.New("consult: incomplete App Server conversation")
	errAppServerAnswer      = errors.New("consult: invalid App Server answer")
)

var (
	errAppServerProtocolInitialize = fmt.Errorf("%w: invalid-protocol-initialize", errAppServerProtocol)
	errAppServerProtocolDiscovery  = fmt.Errorf("%w: invalid-protocol-discovery", errAppServerProtocol)
	errAppServerProtocolThread     = fmt.Errorf("%w: invalid-protocol-thread", errAppServerProtocol)
	errAppServerProtocolTurn       = fmt.Errorf("%w: invalid-protocol-turn", errAppServerProtocol)
	errAppServerProtocolShutdown   = fmt.Errorf("%w: invalid-protocol-shutdown", errAppServerProtocol)
	errAppServerConfigWarning      = fmt.Errorf("%w: config-warning-rejected", errAppServerProtocol)
	errAppServerWarning            = fmt.Errorf("%w: warning-rejected", errAppServerProtocol)
	errAppServerDeprecationNotice  = fmt.Errorf("%w: deprecation-notice-rejected", errAppServerProtocol)
	errAppServerAccountUpdated     = fmt.Errorf("%w: account-updated-rejected", errAppServerProtocol)
)

// A fault carries only a local discriminator and the existing phase sentinel.
// Keep its Error text phase-only; the public mapper owns the closed vocabulary.
type appServerFault struct {
	phase error
	point string
}

func (e *appServerFault) Error() string { return e.phase.Error() }
func (e *appServerFault) Unwrap() error { return e.phase }

// Only these sanitized scalar facts can leave the private exchange. Host exit,
// EOF and cleanup facts must also pass before the caller can admit them.
type appServerResult struct {
	Answer       string
	Usage        usage
	UsagePresent bool
}

type appServerItem struct {
	kind      string
	completed bool
}

type appServerStream struct {
	model, prompt, cwd                                                   string
	nextID, pendingID, interruptID                                       int
	pendingMethod                                                        string
	initialized, remote, threadRequested, threadReply, threadStarted     bool
	turnRequested, turnReply, turnStarted, terminal, closed, interrupted bool
	threadID, turnID                                                     string
	threadIdentity                                                       [4]string
	pages, models                                                        int
	cursors                                                              map[string]bool
	items                                                                map[string]appServerItem
	answer                                                               string
	answerHash                                                           [32]byte
	usagePresent                                                         bool
	total, last                                                          [6]int64
	window                                                               int64
	err                                                                  error
}

func (s *appServerStream) start(cwd string) (duplexAction, error) {
	if s.nextID != 0 || !supportedModel(codexAdapter, s.model) || s.prompt == "" || len(s.prompt) > maxStdinBytes || !utf8.ValidString(s.prompt) || !appAbsolute(cwd) {
		return s.rejectPoint("start-state")
	}
	s.cwd = cwd
	s.cursors = make(map[string]bool)
	s.items = make(map[string]appServerItem)
	return s.request("initialize", map[string]any{
		"clientInfo": map[string]any{"name": "go-llm-consult", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": false, "requestAttestation": false,
			"optOutNotificationMethods": []string{"item/agentMessage/delta", "item/reasoning/textDelta", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/plan/delta"}},
	}), nil
}

func appEncode(v any) []byte {
	// All outbound values are fixed strings, validated strings, integers or
	// their closed containers; none can make encoding/json fail.
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

func (s *appServerStream) request(method string, params any) duplexAction {
	s.nextID++
	s.pendingID, s.pendingMethod = s.nextID, method
	return duplexAction{write: appEncode(struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{s.nextID, method, params})}
}

func (s *appServerStream) reject(err error) (duplexAction, error) {
	if s.err == nil {
		if err == errAppServerProtocol {
			err = s.protocolError()
		}
		s.err = err
	}
	return duplexAction{closeStdin: true}, s.err
}

func (s *appServerStream) rejectPoint(point string) (duplexAction, error) {
	return s.reject(&appServerFault{phase: s.protocolError(), point: point})
}

// These validators are used only on paths that reject when they return false.
// Record the first fault in place rather than re-running stateful validation.
func (s *appServerStream) invalid(point string) bool {
	_, _ = s.rejectPoint(point)
	return false
}

// Derive diagnostics from the existing exchange state, before later records can
// change it. Remote metadata is still initialization; terminal alone is not close.
func (s *appServerStream) protocolError() error {
	switch {
	case s.closed:
		return errAppServerProtocolShutdown
	case s.turnRequested:
		return errAppServerProtocolTurn
	case s.threadRequested:
		return errAppServerProtocolThread
	case s.remote:
		return errAppServerProtocolDiscovery
	default:
		return errAppServerProtocolInitialize
	}
}

func (s *appServerStream) receive(frame []byte) (duplexAction, error) {
	if s.err != nil {
		return duplexAction{closeStdin: true}, s.err
	}
	// B2 owns cumulative bytes, LF assembly and records. Each borrowed callback
	// frame is bounded before the existing depth/duplicate-key decoder runs.
	if len(frame) > streamCap || strings.ContainsRune(string(frame), '\n') {
		return s.rejectPoint("record-framing")
	}
	m, ok := decodeRecord(frame)
	if !ok {
		return s.rejectPoint("record-decode")
	}
	if !appArraysBounded(m) {
		return s.rejectPoint("record-array-bound")
	}
	if s.nextID == 0 {
		return s.rejectPoint("record-before-start")
	}
	_, hasID := m["id"]
	_, hasMethod := m["method"]
	_, hasResult := m["result"]
	_, hasError := m["error"]
	switch {
	case hasMethod && !hasResult && !hasError:
		if !nonempty(m["method"]) {
			return s.rejectPoint("method-shape")
		}
		if hasID {
			if _, ok := appObject(m, "id method", "params trace"); !ok || !appRPCID(m["id"]) || !appTrace(m["trace"]) {
				return s.rejectPoint("request-envelope")
			}
			return s.serverRequest(m)
		}
		if _, ok := appObject(m, "method params", "emittedAtMs"); !ok || !appNullableInt(m["emittedAtMs"]) {
			return s.rejectPoint("notification-envelope")
		}
		return s.notification(m["method"].(string), m["params"])
	case hasID && !hasMethod && hasResult != hasError:
		field := "result"
		if hasError {
			field = "error"
		}
		if !keys(m, "id", field) || !appRPCID(m["id"]) {
			return s.rejectPoint("reply-envelope")
		}
		n, number := m["id"].(json.Number)
		if !number {
			return s.rejectPoint("reply-id")
		}
		if s.interruptID != 0 && string(n) == strconv.Itoa(s.interruptID) {
			s.interruptID = 0
			if hasError || !appEmpty(m["result"]) {
				return s.rejectPoint("interrupt-reply")
			}
			return duplexAction{}, nil
		}
		if s.pendingID == 0 || string(n) != strconv.Itoa(s.pendingID) {
			return s.rejectPoint("reply-correlation")
		}
		if hasError {
			e, ok := appObject(m["error"], "code message", "data")
			if !ok || !appSigned(e["code"], 64) || !stringValue(e["message"]) {
				return s.rejectPoint("rpc-error-shape")
			}
			return s.reject(errAppServerVendor)
		}
		method := s.pendingMethod
		s.pendingID, s.pendingMethod = 0, ""
		return s.reply(method, m["result"])
	default:
		return s.rejectPoint("record-envelope")
	}
}

func (s *appServerStream) reply(method string, value any) (duplexAction, error) {
	switch method {
	case "initialize":
		m, ok := appObject(value, "userAgent codexHome platformFamily platformOs", "")
		if !ok || !nonempty(m["userAgent"]) || !appAbsolute(m["codexHome"]) || m["platformFamily"] != "unix" || !enum(m["platformOs"], "macos", "linux") {
			return s.rejectPoint("initialize-reply")
		}
		s.initialized = true
		return duplexAction{write: []byte("{\"method\":\"initialized\"}\n")}, nil
	case "model/list":
		m, ok := appObject(value, "data", "nextCursor")
		a, array := m["data"].([]any)
		if !ok || !array || len(a) > 100 || !nullableString(m["nextCursor"]) {
			return s.rejectPoint("model-list-shape")
		}
		s.pages++
		s.models += len(a)
		if s.pages > 32 || s.models > 3200 {
			return s.rejectPoint("model-list-bound")
		}
		found := false
		for _, v := range a {
			if !appModel(v) {
				return s.rejectPoint("model-shape")
			}
			if v.(map[string]any)["model"] == s.model {
				found = true
			}
		}
		cursor, more := m["nextCursor"].(string)
		if more && (cursor == "" || len(cursor) > 128 || s.cursors[cursor] || len(a) == 0) {
			return s.rejectPoint("model-cursor")
		}
		if found {
			s.threadRequested = true
			return s.request("thread/start", map[string]any{"model": s.model, "cwd": s.cwd, "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": "read-only", "ephemeral": true}), nil
		}
		if !more {
			return s.reject(errAppServerUnavailable)
		}
		if s.pages == 32 {
			return s.rejectPoint("model-page-bound")
		}
		s.cursors[cursor] = true
		return s.request("model/list", map[string]any{"includeHidden": true, "limit": 100, "cursor": cursor}), nil
	case "thread/start":
		m, ok := appObject(value, "thread model modelProvider cwd approvalPolicy approvalsReviewer sandbox runtimeWorkspaceRoots activePermissionProfile multiAgentMode", "serviceTier reasoningEffort instructionSources")
		if !ok {
			return s.rejectPoint("thread-reply-shape")
		}
		if m["model"] != s.model || m["cwd"] != s.cwd || m["approvalPolicy"] != "never" || m["approvalsReviewer"] != "user" || !nonempty(m["modelProvider"]) || !appReadOnly(m["sandbox"]) || !appList(m["runtimeWorkspaceRoots"], appAbsolute) || !appProfile(m["activePermissionProfile"]) || m["multiAgentMode"] != "explicitRequestOnly" || !nullableString(m["serviceTier"]) || !appEffort(m["reasoningEffort"]) {
			return s.rejectPoint("thread-reply-profile")
		}
		if v, has := m["instructionSources"]; has && !stringArray(v) {
			return s.rejectPoint("thread-instructions")
		}
		identity, valid := s.thread(m["thread"])
		if !valid || identity[3] != m["modelProvider"] {
			return s.rejectPoint("thread-reply-identity")
		}
		s.threadIdentity, s.threadReply = identity, true
	case "turn/start":
		m, ok := appObject(value, "turn", "")
		if !ok {
			return s.rejectPoint("turn-reply-shape")
		}
		if !s.turn(m["turn"], false) {
			return s.reject(errAppServerProtocol)
		}
		s.turnReply = true
	default:
		return s.rejectPoint("reply-method")
	}
	return s.advance()
}

func (s *appServerStream) advance() (duplexAction, error) {
	if s.threadReply && s.threadStarted && !s.turnRequested {
		s.turnRequested = true
		return s.request("turn/start", map[string]any{"threadId": s.threadID, "input": []any{map[string]any{"type": "text", "text": s.prompt, "text_elements": []any{}}}}), nil
	}
	if s.terminal && s.turnReply && !s.closed && !s.interrupted {
		s.closed = true
		return duplexAction{closeStdin: true}, nil
	}
	return duplexAction{}, nil
}

func (s *appServerStream) notification(method string, value any) (duplexAction, error) {
	if !s.initialized {
		return s.rejectPoint("notification-before-initialize")
	}
	valid, point := false, "notification-shape"
	switch method {
	case "configWarning":
		return s.reject(errAppServerConfigWarning)
	case "warning":
		return s.reject(errAppServerWarning)
	case "deprecationNotice":
		return s.reject(errAppServerDeprecationNotice)
	case "account/updated":
		return s.reject(errAppServerAccountUpdated)
	case "error":
		// This label asserts only the rejected method, not the payload's validity.
		return s.rejectPoint("error-notification-rejected")
	case "remoteControl/status/changed":
		m, ok := appObject(value, "status serverName installationId", "environmentId")
		valid = ok && m["status"] == "disabled" && stringValue(m["serverName"]) && stringValue(m["installationId"]) && m["environmentId"] == nil
		point = "remote-status"
		if valid && !s.remote {
			s.remote = true
			return s.request("model/list", map[string]any{"includeHidden": true, "limit": 100}), nil
		}
	case "skills/changed":
		valid, point = appEmpty(value), "skills-shape"
	case "account/rateLimits/updated":
		m, ok := appObject(value, "rateLimits", "")
		valid, point = ok && appRateLimits(m["rateLimits"]), "rate-limits-shape"
	case "thread/started":
		m, ok := appObject(value, "thread", "")
		if !ok {
			break
		}
		if !s.threadReply || s.threadStarted || s.turnRequested {
			return s.rejectPoint("notification-order")
		}
		identity, checked := s.thread(m["thread"])
		valid, point = checked && identity == s.threadIdentity, "thread-started-identity"
		if valid {
			s.threadStarted = true
		}
	case "thread/status/changed":
		m, ok := appObject(value, "threadId status", "")
		if !ok {
			break
		}
		if !s.matchThread(m["threadId"], true) {
			return s.rejectPoint("thread-correlation")
		}
		if status, ok := appObject(m["status"], "type", ""); ok && status["type"] == "systemError" {
			return s.rejectPoint("system-error-rejected")
		}
		valid, point = appThreadStatus(m["status"], s.turnRequested, s.turnID == "" || s.terminal), "thread-status"
	case "thread/name/updated":
		m, ok := appObject(value, "threadId", "threadName")
		if !ok {
			break
		}
		if !s.matchThread(m["threadId"], false) {
			return s.rejectPoint("thread-correlation")
		}
		valid, point = nullableString(m["threadName"]), "thread-name"
	case "thread/closed":
		m, ok := appObject(value, "threadId", "")
		if !ok {
			break
		}
		if !s.closed {
			return s.rejectPoint("notification-order")
		}
		valid, point = s.matchThread(m["threadId"], false), "thread-correlation"
	case "mcpServer/startupStatus/updated":
		m, ok := appObject(value, "name status", "threadId error failureReason")
		if !ok {
			return s.rejectPoint("mcp-startup-shape")
		}
		if !stringValue(m["name"]) {
			return s.rejectPoint("mcp-startup-name")
		}
		if !enum(m["status"], "starting", "ready", "cancelled") {
			if m["status"] == "failed" {
				return s.rejectPoint("mcp-startup-failed")
			}
			return s.rejectPoint("mcp-startup-status")
		}
		if m["error"] != nil {
			return s.rejectPoint("mcp-startup-error")
		}
		if m["failureReason"] != nil {
			return s.rejectPoint("mcp-startup-failure-reason")
		}
		valid = m["threadId"] == nil || s.matchThread(m["threadId"], true)
		point = "mcp-startup-thread"
	case "turn/started":
		m, ok := appObject(value, "threadId turn", "")
		if !ok {
			break
		}
		if s.turnStarted || s.terminal {
			return s.rejectPoint("notification-order")
		}
		if !s.matchThread(m["threadId"], false) {
			return s.rejectPoint("thread-correlation")
		}
		valid = s.turn(m["turn"], false)
		if valid {
			s.turnStarted = true
		}
	case "item/started", "item/completed":
		timing := "startedAtMs"
		if method == "item/completed" {
			timing = "completedAtMs"
		}
		m, ok := appObject(value, "threadId turnId item "+timing, "")
		if !ok {
			break
		}
		if !s.active(m) {
			return s.rejectPoint("active-correlation")
		}
		if !appCounter(m[timing]) {
			return s.rejectPoint("item-timing")
		}
		valid = s.item(m["item"], method == "item/completed")
	case "thread/tokenUsage/updated":
		m, ok := appObject(value, "threadId turnId tokenUsage", "")
		if !ok {
			break
		}
		if !s.active(m) {
			return s.rejectPoint("active-correlation")
		}
		valid = s.tokenUsage(m["tokenUsage"])
	case "model/verification":
		m, ok := appObject(value, "threadId turnId verifications", "")
		if !ok {
			break
		}
		if !s.active(m) {
			return s.rejectPoint("active-correlation")
		}
		valid, point = appList(m["verifications"], func(v any) bool { return v == "trustedAccessForCyber" }), "model-verification"
	case "model/safetyBuffering/updated":
		m, ok := appObject(value, "threadId turnId model reasons useCases showBufferingUi", "fasterModel")
		if !ok {
			break
		}
		if !s.active(m) {
			return s.rejectPoint("active-correlation")
		}
		valid = m["model"] == s.model && stringArray(m["reasons"]) && stringArray(m["useCases"]) && appBool(m["showBufferingUi"]) && nullableString(m["fasterModel"])
		point = "model-safety-buffering"
	case "turn/completed":
		m, ok := appObject(value, "threadId turn", "")
		if !ok {
			break
		}
		if !s.turnStarted || s.terminal {
			return s.rejectPoint("notification-order")
		}
		if !s.matchThread(m["threadId"], false) {
			return s.rejectPoint("thread-correlation")
		}
		valid = s.turn(m["turn"], true)
		if valid {
			for _, item := range s.items {
				if !item.completed {
					return s.rejectPoint("terminal-items-incomplete")
				}
			}
			if s.answer == "" {
				return s.rejectPoint("terminal-answer-missing")
			}
			if len(sanitize(s.answer)) > MaxAnswerBytes {
				return s.reject(errAppServerAnswer)
			}
			s.terminal = true
		}
	default:
		return s.rejectPoint(appRejectedNotification(method))
	}
	if !valid {
		return s.rejectPoint(point)
	}
	return s.advance()
}

// Known rejected methods and item kinds map to literals; unknown wire values
// never become diagnostic text. These switches do not admit any new records.
func appRejectedNotification(method string) string {
	switch method {
	case "thread/archived":
		return "thread-archived-rejected"
	case "thread/deleted":
		return "thread-deleted-rejected"
	case "thread/unarchived":
		return "thread-unarchived-rejected"
	case "thread/reverted":
		return "thread-reverted-rejected"
	case "thread/goal/updated":
		return "thread-goal-updated-rejected"
	case "thread/goal/cleared":
		return "thread-goal-cleared-rejected"
	case "thread/queue/changed":
		return "thread-queue-changed-rejected"
	case "project/changed":
		return "project-changed-rejected"
	case "thread/project/updated":
		return "thread-project-updated-rejected"
	case "thread/environment/connected":
		return "thread-environment-connected-rejected"
	case "thread/environment/disconnected":
		return "thread-environment-disconnected-rejected"
	case "thread/settings/updated":
		return "thread-settings-updated-rejected"
	case "hook/started":
		return "hook-started-rejected"
	case "hook/completed":
		return "hook-completed-rejected"
	case "turn/diff/updated":
		return "turn-diff-updated-rejected"
	case "turn/plan/updated":
		return "turn-plan-updated-rejected"
	case "item/autoApprovalReview/started":
		return "auto-approval-review-started-rejected"
	case "item/autoApprovalReview/completed":
		return "auto-approval-review-completed-rejected"
	case "autoApprovalReview/strictReviewRequired":
		return "auto-approval-review-strict-review-required-rejected"
	case "rawResponseItem/completed":
		return "raw-response-item-completed-rejected"
	case "rawResponse/completed":
		return "raw-response-completed-rejected"
	case "item/agentMessage/delta":
		return "agent-message-delta-rejected"
	case "item/plan/delta":
		return "plan-delta-rejected"
	case "command/exec/outputDelta":
		return "command-exec-output-delta-rejected"
	case "process/outputDelta":
		return "process-output-delta-rejected"
	case "process/exited":
		return "process-exited-rejected"
	case "item/commandExecution/outputDelta":
		return "command-execution-output-delta-rejected"
	case "item/commandExecution/terminalInteraction":
		return "command-execution-terminal-interaction-rejected"
	case "item/fileChange/outputDelta":
		return "file-change-output-delta-rejected"
	case "item/fileChange/patchUpdated":
		return "file-change-patch-updated-rejected"
	case "serverRequest/resolved":
		return "server-request-resolved-rejected"
	case "item/mcpToolCall/progress":
		return "mcp-tool-call-progress-rejected"
	case "mcpServer/oauthLogin/completed":
		return "mcp-server-oauth-login-completed-rejected"
	case "mcpServer/event/stream/notification":
		return "mcp-server-event-stream-notification-rejected"
	case "app/list/updated":
		return "app-list-updated-rejected"
	case "externalAgentConfig/import/progress":
		return "external-agent-config-import-progress-rejected"
	case "externalAgentConfig/import/completed":
		return "external-agent-config-import-completed-rejected"
	case "fs/changed":
		return "fs-changed-rejected"
	case "item/reasoning/summaryTextDelta":
		return "reasoning-summary-text-delta-rejected"
	case "item/reasoning/summaryPartAdded":
		return "reasoning-summary-part-added-rejected"
	case "item/reasoning/textDelta":
		return "reasoning-text-delta-rejected"
	case "thread/compacted":
		return "thread-compacted-rejected"
	case "model/rerouted":
		return "model-rerouted-rejected"
	case "modelProvider/authRecoveryStarted":
		return "model-provider-auth-recovery-started-rejected"
	case "modelProvider/authRecoveryCompleted":
		return "model-provider-auth-recovery-completed-rejected"
	case "turn/moderationMetadata":
		return "turn-moderation-metadata-rejected"
	case "guardianWarning":
		return "guardian-warning-rejected"
	case "fuzzyFileSearch/sessionUpdated":
		return "fuzzy-file-search-session-updated-rejected"
	case "fuzzyFileSearch/sessionCompleted":
		return "fuzzy-file-search-session-completed-rejected"
	case "thread/realtime/started":
		return "thread-realtime-started-rejected"
	case "thread/realtime/itemAdded":
		return "thread-realtime-item-added-rejected"
	case "thread/realtime/item/started":
		return "thread-realtime-item-started-rejected"
	case "thread/realtime/item/transcript/delta":
		return "thread-realtime-item-transcript-delta-rejected"
	case "thread/realtime/item/completed":
		return "thread-realtime-item-completed-rejected"
	case "thread/realtime/transcript/delta":
		return "thread-realtime-transcript-delta-rejected"
	case "thread/realtime/transcript/done":
		return "thread-realtime-transcript-done-rejected"
	case "thread/realtime/outputAudio/delta":
		return "thread-realtime-output-audio-delta-rejected"
	case "thread/realtime/sdp":
		return "thread-realtime-sdp-rejected"
	case "thread/realtime/error":
		return "thread-realtime-error-rejected"
	case "thread/realtime/closed":
		return "thread-realtime-closed-rejected"
	case "windows/worldWritableWarning":
		return "windows-world-writable-warning-rejected"
	case "windowsSandbox/setupCompleted":
		return "windows-sandbox-setup-completed-rejected"
	case "account/login/completed":
		return "account-login-completed-rejected"
	default:
		return "notification-unknown"
	}
}

func appRejectedItem(kind any) string {
	switch kind {
	case "hookPrompt":
		return "item-hook-prompt-rejected"
	case "functionCallOutput":
		return "item-function-call-output-rejected"
	case "plan":
		return "item-plan-rejected"
	case "commandExecution":
		return "item-command-execution-rejected"
	case "fileChange":
		return "item-file-change-rejected"
	case "mcpToolCall":
		return "item-mcp-tool-call-rejected"
	case "dynamicToolCall":
		return "item-dynamic-tool-call-rejected"
	case "collabAgentToolCall":
		return "item-collab-agent-tool-call-rejected"
	case "subAgentActivity":
		return "item-sub-agent-activity-rejected"
	case "webSearch":
		return "item-web-search-rejected"
	case "imageView":
		return "item-image-view-rejected"
	case "sleep":
		return "item-sleep-rejected"
	case "imageGeneration":
		return "item-image-generation-rejected"
	case "enteredReviewMode":
		return "item-entered-review-mode-rejected"
	case "exitedReviewMode":
		return "item-exited-review-mode-rejected"
	case "contextCompaction":
		return "item-context-compaction-rejected"
	default:
		return "item-unknown"
	}
}

func (s *appServerStream) matchThread(value any, tentative bool) bool {
	if !s.threadRequested || !appID(value) {
		return false
	}
	id := value.(string)
	if s.threadID == "" && tentative {
		s.threadID = id
	}
	return id == s.threadID
}

func (s *appServerStream) active(m map[string]any) bool {
	return s.turnStarted && !s.terminal && s.matchThread(m["threadId"], false) && appID(m["turnId"]) && m["turnId"] == s.turnID
}

func (s *appServerStream) turn(value any, terminal bool) bool {
	m, ok := appObject(value, "id items status", "itemsView error startedAt completedAt durationMs")
	if !ok {
		return s.invalid("turn-shape")
	}
	if !s.turnRequested {
		return s.invalid("turn-order")
	}
	if !appID(m["id"]) {
		return s.invalid("turn-id")
	}
	if m["error"] != nil {
		return s.invalid("turn-error")
	}
	if !appNullableInt(m["startedAt"]) || !appNullableInt(m["completedAt"]) || !appNullableInt(m["durationMs"]) {
		return s.invalid("turn-timing")
	}
	if s.turnID != "" && m["id"] != s.turnID {
		return s.invalid("turn-id")
	}
	a, ok := m["items"].([]any)
	if !ok {
		return s.invalid("turn-items")
	}
	if !terminal {
		if m["status"] != "inProgress" {
			return s.invalid("turn-status")
		}
		if m["itemsView"] != "notLoaded" {
			return s.invalid("turn-view")
		}
		if len(a) != 0 {
			return s.invalid("turn-items")
		}
	} else {
		if m["status"] != "completed" {
			return s.invalid("turn-status")
		}
		switch m["itemsView"] {
		case "notLoaded":
			if len(a) != 0 {
				return s.invalid("terminal-items")
			}
		case "summary":
			if len(a) != 1 {
				return s.invalid("terminal-summary")
			}
			if s.answer == "" {
				return s.invalid("terminal-answer-missing")
			}
			item, valid := s.validItem(a[0])
			if !valid {
				return false
			}
			if item["type"] != "agentMessage" || appAgentHash(item) != s.answerHash {
				return s.invalid("terminal-summary-mismatch")
			}
		default:
			return s.invalid("turn-view")
		}
	}
	s.turnID = m["id"].(string)
	return true
}

func (s *appServerStream) item(value any, completed bool) bool {
	m, ok := s.validItem(value)
	if !ok {
		return false
	}
	id, kind := m["id"].(string), m["type"].(string)
	old, exists := s.items[id]
	if exists && (old.completed || !completed || old.kind != kind) {
		return s.invalid("item-lifecycle")
	}
	if !exists && len(s.items) >= recordCap {
		return s.invalid("item-bound")
	}
	s.items[id] = appServerItem{kind: kind, completed: completed}
	if completed && kind == "agentMessage" && m["phase"] != "commentary" && strings.TrimSpace(m["text"].(string)) != "" {
		s.answer = m["text"].(string)
		s.answerHash = appAgentHash(m)
	}
	return true
}

func appAgentHash(m map[string]any) [32]byte {
	// Normalize absent/default-null metadata for the terminal summary check.
	// Retain only its digest, never citation paths or question/reasoning text.
	for _, key := range []string{"phase", "memoryCitation", "delivery", "questions"} {
		if _, ok := m[key]; !ok {
			m[key] = nil
		}
	}
	if questions, ok := m["questions"].([]any); ok {
		for _, v := range questions {
			q := v.(map[string]any)
			if _, ok := q["options"]; !ok {
				q["options"] = nil
			}
		}
	}
	return sha256.Sum256(appEncode(m))
}

func (s *appServerStream) finish() (appServerResult, error) {
	if s.err != nil {
		return appServerResult{}, s.err
	}
	if !s.closed || s.interrupted {
		return appServerResult{}, errAppServerIncomplete
	}
	answer := sanitize(s.answer)
	if strings.TrimSpace(answer) == "" || len(answer) > MaxAnswerBytes {
		return appServerResult{}, errAppServerAnswer
	}
	return appServerResult{Answer: answer, UsagePresent: s.usagePresent,
		Usage: usage{Input: s.total[1], Cached: s.total[2], CacheWrite: s.total[3], Output: s.total[4], Reasoning: s.total[5]}}, nil
}

func (s *appServerStream) interrupt() []byte {
	if s.interrupted {
		return nil
	}
	s.interrupted = true
	if s.err != nil || s.terminal || s.threadID == "" || s.turnID == "" {
		return nil
	}
	s.nextID++
	s.interruptID = s.nextID
	return appEncode(struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{s.interruptID, "turn/interrupt", map[string]any{"threadId": s.threadID, "turnId": s.turnID}})
}

// The following validators implement only the pinned consultation subset.
// appObject closes a single known object, not an extensible schema mechanism.
func appObject(value any, required, optional string) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	for _, k := range strings.Fields(required) {
		if _, ok := m[k]; !ok {
			return nil, false
		}
	}
	allowed := strings.Fields(required + " " + optional)
	for k := range m {
		found := false
		for _, name := range allowed {
			if k == name {
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return m, true
}
func appEmpty(value any) bool       { _, ok := appObject(value, "", ""); return ok }
func appBool(value any) bool        { _, ok := value.(bool); return ok }
func appCounter(value any) bool     { _, ok := boundedInt(value); return ok }
func appNullableInt(value any) bool { return value == nil || appCounter(value) }
func appEffort(value any) bool      { return value == nil || nonempty(value) }
func appAbsolute(value any) bool    { s, ok := value.(string); return ok && strings.HasPrefix(s, "/") }
func appSigned(value any, bits int) bool {
	n, ok := value.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return false
	}
	_, err := strconv.ParseInt(string(n), 10, bits)
	return err == nil
}
func appID(value any) bool {
	s, ok := value.(string)
	return ok && len(s) > 0 && len(s) <= 256 && strings.IndexFunc(s, unicode.IsControl) < 0
}
func appRPCID(value any) bool {
	if s, ok := value.(string); ok {
		return len(s) <= 256
	}
	return appSigned(value, 64)
}
func appList(value any, valid func(any) bool) bool {
	a, ok := value.([]any)
	if !ok || len(a) > recordCap {
		return false
	}
	for _, v := range a {
		if !valid(v) {
			return false
		}
	}
	return true
}
func appArraysBounded(value any) bool {
	switch v := value.(type) {
	case []any:
		if len(v) > recordCap {
			return false
		}
		for _, x := range v {
			if !appArraysBounded(x) {
				return false
			}
		}
	case map[string]any:
		for _, x := range v {
			if !appArraysBounded(x) {
				return false
			}
		}
	}
	return true
}
func appTrace(value any) bool {
	if value == nil {
		return true
	}
	m, ok := appObject(value, "", "traceparent tracestate")
	return ok && nullableString(m["traceparent"]) && nullableString(m["tracestate"])
}
func appReadOnly(value any) bool {
	m, ok := appObject(value, "type", "networkAccess")
	v, has := m["networkAccess"]
	return ok && m["type"] == "readOnly" && (!has || v == false)
}
func appProfile(value any) bool {
	if value == nil {
		return true
	}
	m, ok := appObject(value, "id", "extends")
	return ok && stringValue(m["id"]) && nullableString(m["extends"])
}
func appThreadStatus(value any, active, notLoaded bool) bool {
	m, ok := appObject(value, "type", "activeFlags")
	if !ok {
		return false
	}
	if m["type"] == "active" {
		a, ok := m["activeFlags"].([]any)
		return active && ok && len(a) == 0
	}
	return keys(m, "type") && (m["type"] == "idle" || notLoaded && m["type"] == "notLoaded")
}

func appModel(value any) bool {
	m, ok := appObject(value, "id model displayName description hidden isDefault defaultReasoningEffort supportedReasoningEfforts", "additionalSpeedTiers availabilityNux defaultServiceTier inputModalities modelSpecialty multiAgentVersion serviceTiers supportsPersonality upgrade upgradeInfo")
	if !ok || !appBool(m["hidden"]) || !appBool(m["isDefault"]) || !nonempty(m["defaultReasoningEffort"]) {
		return false
	}
	for _, k := range []string{"id", "model", "displayName", "description"} {
		if !stringValue(m[k]) {
			return false
		}
	}
	for _, k := range []string{"defaultServiceTier", "modelSpecialty", "upgrade"} {
		if !nullableString(m[k]) {
			return false
		}
	}
	if !appList(m["supportedReasoningEfforts"], func(v any) bool {
		x, ok := appObject(v, "reasoningEffort description", "")
		return ok && nonempty(x["reasoningEffort"]) && stringValue(x["description"])
	}) {
		return false
	}
	if v, has := m["supportsPersonality"]; has && !appBool(v) {
		return false
	}
	if v, has := m["additionalSpeedTiers"]; has && !stringArray(v) {
		return false
	}
	if v, has := m["inputModalities"]; has && !appList(v, func(x any) bool { return enum(x, "text", "image", "audio") }) {
		return false
	}
	if v := m["multiAgentVersion"]; v != nil && !enum(v, "disabled", "v1", "v2") {
		return false
	}
	if v := m["availabilityNux"]; v != nil {
		x, ok := appObject(v, "message", "")
		if !ok || !stringValue(x["message"]) {
			return false
		}
	}
	if v, has := m["serviceTiers"]; has && !appList(v, func(v any) bool {
		x, ok := appObject(v, "id name description", "")
		return ok && stringValue(x["id"]) && stringValue(x["name"]) && stringValue(x["description"])
	}) {
		return false
	}
	if v := m["upgradeInfo"]; v != nil {
		x, ok := appObject(v, "model", "migrationMarkdown modelLink retirementAt upgradeCopy")
		if !ok || !stringValue(x["model"]) || !nullableString(x["migrationMarkdown"]) || !nullableString(x["modelLink"]) || !nullableString(x["upgradeCopy"]) || !appNullableInt(x["retirementAt"]) {
			return false
		}
	}
	return true
}

func (s *appServerStream) thread(value any) ([4]string, bool) {
	var identity [4]string
	m, ok := appObject(value, "id sessionId modelProvider cwd source ephemeral turns status cliVersion createdAt updatedAt preview projectId extra canAcceptDirectInput", "agentNickname agentRole forkedFromId gitInfo historyMode model name parentThreadId path reasoningEffort recencyAt section sectionEnteredAt threadSource")
	if !ok || !s.matchThread(m["id"], true) || !appID(m["sessionId"]) || !nonempty(m["modelProvider"]) || m["cwd"] != s.cwd || m["source"] != "vscode" || m["ephemeral"] != true || m["canAcceptDirectInput"] != true || !appThreadStatus(m["status"], false, true) {
		return identity, false
	}
	turns, ok := m["turns"].([]any)
	if !ok || len(turns) != 0 || (m["extra"] != nil && !appEmpty(m["extra"])) || m["forkedFromId"] != nil || m["parentThreadId"] != nil || !appEffort(m["reasoningEffort"]) {
		return identity, false
	}
	if v, has := m["historyMode"]; has && v != "legacy" {
		return identity, false
	}
	if v := m["model"]; v != nil && v != s.model {
		return identity, false
	}
	for _, k := range []string{"cliVersion", "preview"} {
		if !stringValue(m[k]) {
			return identity, false
		}
	}
	for _, k := range []string{"agentNickname", "agentRole", "name", "path", "projectId", "threadSource"} {
		if !nullableString(m[k]) {
			return identity, false
		}
	}
	for _, k := range []string{"createdAt", "updatedAt"} {
		if !appCounter(m[k]) {
			return identity, false
		}
	}
	for _, k := range []string{"recencyAt", "sectionEnteredAt"} {
		if !appNullableInt(m[k]) {
			return identity, false
		}
	}
	if v := m["gitInfo"]; v != nil {
		x, ok := appObject(v, "", "branch originUrl sha")
		if !ok || !nullableString(x["branch"]) || !nullableString(x["originUrl"]) || !nullableString(x["sha"]) {
			return identity, false
		}
	}
	if v := m["section"]; v != nil {
		x, ok := appObject(v, "id name", "appearance")
		if !ok || !stringValue(x["id"]) || !stringValue(x["name"]) {
			return identity, false
		}
		if v := x["appearance"]; v != nil {
			y, ok := appObject(v, "", "color icon")
			if !ok || !nullableString(y["color"]) || !nullableString(y["icon"]) {
				return identity, false
			}
		}
	}
	identity = [4]string{m["id"].(string), m["sessionId"].(string), "", m["modelProvider"].(string)}
	identity[2], _ = m["model"].(string)
	return identity, true
}

func (s *appServerStream) validItem(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	if !ok || !appID(m["id"]) {
		return nil, s.invalid("item-shape")
	}
	switch m["type"] {
	case "userMessage":
		if _, ok := appObject(m, "type id content", "clientId"); !ok || m["clientId"] != nil {
			return nil, s.invalid("item-user-shape")
		}
		a, ok := m["content"].([]any)
		if !ok || len(a) != 1 {
			return nil, s.invalid("item-user-content")
		}
		x, ok := appObject(a[0], "type text", "text_elements")
		if !ok || x["type"] != "text" {
			return nil, s.invalid("item-user-content")
		}
		if x["text"] != s.prompt {
			return nil, s.invalid("item-prompt-echo")
		}
		if v, has := x["text_elements"]; has {
			a, ok := v.([]any)
			if !ok || len(a) != 0 {
				return nil, s.invalid("item-user-elements")
			}
		}
	case "reasoning":
		if _, ok := appObject(m, "type id", "summary content"); !ok {
			return nil, s.invalid("item-reasoning-shape")
		}
		for _, k := range []string{"summary", "content"} {
			if v, has := m[k]; has && !stringArray(v) {
				return nil, s.invalid("item-reasoning-content")
			}
		}
	case "agentMessage":
		if _, ok := appObject(m, "type id text", "phase memoryCitation delivery questions"); !ok || !stringValue(m["text"]) || (m["phase"] != nil && !enum(m["phase"], "commentary", "final_answer")) || (m["delivery"] != nil && m["delivery"] != "async") {
			return nil, s.invalid("item-agent-shape")
		}
		if v := m["memoryCitation"]; v != nil {
			x, ok := appObject(v, "entries threadIds", "")
			if !ok || !appList(x["threadIds"], appID) || !appList(x["entries"], func(v any) bool {
				y, ok := appObject(v, "lineStart lineEnd note path", "")
				a, aok := boundedInt(y["lineStart"])
				b, bok := boundedInt(y["lineEnd"])
				return ok && aok && bok && a <= 4294967295 && b <= 4294967295 && stringValue(y["note"]) && stringValue(y["path"])
			}) {
				return nil, s.invalid("item-agent-citation")
			}
		}
		if v := m["questions"]; v != nil && !appList(v, func(v any) bool {
			x, ok := appObject(v, "title", "options")
			return ok && stringValue(x["title"]) && (x["options"] == nil || stringArray(x["options"]))
		}) {
			return nil, s.invalid("item-agent-questions")
		}
	default:
		return nil, s.invalid(appRejectedItem(m["type"]))
	}
	return m, true
}

func appBreakdown(value any) ([6]int64, string) {
	var result [6]int64
	m, ok := appObject(value, "totalTokens inputTokens cachedInputTokens outputTokens reasoningOutputTokens", "cacheWriteInputTokens")
	if !ok {
		return result, "usage-shape"
	}
	for i, k := range []string{"totalTokens", "inputTokens", "cachedInputTokens", "cacheWriteInputTokens", "outputTokens", "reasoningOutputTokens"} {
		v, has := m[k]
		if !has && k == "cacheWriteInputTokens" {
			continue
		}
		n, ok := boundedInt(v)
		if !ok {
			return result, "usage-counters"
		}
		result[i] = n
	}
	if result[2] > result[1] || result[5] > result[4] {
		return result, "usage-counters"
	}
	return result, ""
}
func (s *appServerStream) tokenUsage(value any) bool {
	m, ok := appObject(value, "total last", "modelContextWindow")
	if !ok {
		return s.invalid("usage-shape")
	}
	total, point := appBreakdown(m["total"])
	if point != "" {
		return s.invalid(point)
	}
	last, point := appBreakdown(m["last"])
	if point != "" {
		return s.invalid(point)
	}
	if !appNullableInt(m["modelContextWindow"]) {
		return s.invalid("usage-window")
	}
	for i, n := range total {
		if last[i] > n {
			return s.invalid("usage-counters")
		}
		if s.usagePresent && n < s.total[i] {
			return s.invalid("usage-monotonic")
		}
	}
	if s.usagePresent && total == s.total && last != s.last {
		return s.invalid("usage-monotonic")
	}
	if v := m["modelContextWindow"]; v != nil {
		n, _ := boundedInt(v)
		if n == 0 || s.window != 0 && n != s.window {
			return s.invalid("usage-window")
		}
		s.window = n
	}
	s.total, s.last, s.usagePresent = total, last, true
	return true
}

func appRateLimits(value any) bool {
	m, ok := appObject(value, "", "credits individualLimit limitId limitName planType primary rateLimitReachedType secondary spendControlReached")
	if !ok || !nullableString(m["limitId"]) || !nullableString(m["limitName"]) {
		return false
	}
	if v := m["spendControlReached"]; v != nil && !appBool(v) {
		return false
	}
	if v := m["planType"]; v != nil && !enum(v, "free", "go", "plus", "pro", "prolite", "team", "self_serve_business_prolite", "self_serve_business_usage_based", "business", "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise", "edu", "edu_plus", "edu_pro", "unknown") {
		return false
	}
	if v := m["rateLimitReachedType"]; v != nil && !enum(v, "rate_limit_reached", "workspace_owner_credits_depleted", "workspace_member_credits_depleted", "workspace_owner_usage_limit_reached", "workspace_member_usage_limit_reached") {
		return false
	}
	for _, k := range []string{"primary", "secondary"} {
		if v := m[k]; v != nil {
			x, ok := appObject(v, "usedPercent", "resetsAt windowDurationMins")
			if !ok || !appSigned(x["usedPercent"], 32) || !appNullableInt(x["resetsAt"]) || !appNullableInt(x["windowDurationMins"]) {
				return false
			}
		}
	}
	if v := m["credits"]; v != nil {
		x, ok := appObject(v, "hasCredits unlimited", "balance")
		if !ok || !appBool(x["hasCredits"]) || !appBool(x["unlimited"]) || !nullableString(x["balance"]) {
			return false
		}
	}
	if v := m["individualLimit"]; v != nil {
		x, ok := appObject(v, "limit used remainingPercent resetsAt", "")
		if !ok || !stringValue(x["limit"]) || !stringValue(x["used"]) || !appSigned(x["remainingPercent"], 32) || !appCounter(x["resetsAt"]) {
			return false
		}
	}
	return true
}

func (s *appServerStream) serverRequest(m map[string]any) (duplexAction, error) {
	reply := map[string]any{"id": m["id"], "error": map[string]any{"code": -32000, "message": "unsupported server request"}}
	if (m["method"] == "item/commandExecution/requestApproval" || m["method"] == "item/fileChange/requestApproval") && s.approval(m["method"].(string), m["params"]) {
		reply = map[string]any{"id": m["id"], "result": map[string]any{"decision": "cancel"}}
	}
	_, err := s.reject(errAppServerRequest)
	return duplexAction{write: appEncode(reply), closeStdin: true}, err
}

func (s *appServerStream) approval(method string, value any) bool {
	optional := "reason grantRoot"
	if method == "item/commandExecution/requestApproval" {
		optional = "approvalId command commandActions cwd environmentId kind networkApprovalContext proposedExecpolicyAmendment proposedNetworkPolicyAmendments reason"
	}
	m, ok := appObject(value, "threadId turnId itemId startedAtMs", optional)
	if !ok || !s.active(m) || !appID(m["itemId"]) || !appCounter(m["startedAtMs"]) {
		return false
	}
	for _, k := range []string{"reason", "grantRoot", "approvalId", "command", "cwd", "environmentId"} {
		if !nullableString(m[k]) {
			return false
		}
	}
	if method == "item/fileChange/requestApproval" {
		return true
	}
	if v, has := m["kind"]; has && !enum(v, "command", "writeStdin") {
		return false
	}
	if v := m["proposedExecpolicyAmendment"]; v != nil && !stringArray(v) {
		return false
	}
	if v := m["networkApprovalContext"]; v != nil {
		x, ok := appObject(v, "host protocol", "")
		if !ok || !stringValue(x["host"]) || !enum(x["protocol"], "http", "https", "socks5Tcp", "socks5Udp") {
			return false
		}
	}
	if v := m["proposedNetworkPolicyAmendments"]; v != nil && !appList(v, func(v any) bool {
		x, ok := appObject(v, "host action", "")
		return ok && stringValue(x["host"]) && enum(x["action"], "allow", "deny")
	}) {
		return false
	}
	if v := m["commandActions"]; v != nil && !appList(v, func(v any) bool {
		x, ok := v.(map[string]any)
		if !ok {
			return false
		}
		switch x["type"] {
		case "read":
			return keys(x, "type", "command", "name", "path") && stringValue(x["command"]) && stringValue(x["name"]) && stringValue(x["path"])
		case "listFiles", "search":
			optional := "path"
			if x["type"] == "search" {
				optional += " query"
			}
			_, ok := appObject(x, "type command", optional)
			return ok && stringValue(x["command"]) && nullableString(x["path"]) && nullableString(x["query"])
		case "unknown":
			return keys(x, "type", "command") && stringValue(x["command"])
		}
		return false
	}) {
		return false
	}
	return true
}
