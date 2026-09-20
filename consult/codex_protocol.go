package consult

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const streamCap = 1048576
const recordCap = 4096

type usage struct{ Input, Cached, CacheWrite, Output, Reasoning int64 }
type streamResult struct {
	Verdict, Answer                     string
	TurnStarted                         bool
	Usage                               usage
	ErrorItemsPreTurn, ErrorItemsInTurn int
	ModelReroutedSeen                   bool
}

// One state machine serves both final admission and the cancellation observer.
// rank is sticky; later malformed data can strengthen a previous rejection.
type codexStream struct {
	result                 streamResult
	rank, records          int
	thread, turn, terminal bool
	ids                    map[string]string // "todo" or an action type is active; "closed" cannot be reused
	answer                 string
}

func (s *codexStream) fail(rank int) {
	if rank > s.rank {
		s.rank = rank
	}
}
func keys(m map[string]any, names ...string) bool {
	if len(m) != len(names) {
		return false
	}
	for _, k := range names {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}
func stringValue(v any) bool { _, ok := v.(string); return ok }
func nonempty(v any) bool    { x, ok := v.(string); return ok && x != "" }
func decodeRecord(line []byte) (map[string]any, bool) {
	if !utf8.Valid(line) {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.UseNumber()
	v, err := claudeJSONValue(d, 0)
	if err != nil {
		return nil, false
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}
func (s *codexStream) record(line []byte) {
	s.records++
	if s.records > recordCap {
		s.fail(3)
		return
	}
	m, ok := decodeRecord(line)
	if !ok || s.terminal {
		s.fail(3)
		return
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "thread.started":
		if s.records != 1 || s.thread || !keys(m, "type", "thread_id") || !nonempty(m["thread_id"]) {
			s.fail(3)
			return
		}
		s.thread = true
	case "turn.started":
		if !s.thread || s.turn || !keys(m, "type") {
			s.fail(3)
			return
		}
		s.turn = true
	case "item.started", "item.updated", "item.completed":
		item, ok := m["item"].(map[string]any)
		if !keys(m, "type", "item") || !ok || !s.thread {
			s.fail(3)
			return
		}
		s.item(typ, item)
	case "turn.completed":
		if !s.turn || !keys(m, "type", "usage") {
			s.fail(3)
			return
		}
		u, ok := readUsage(m["usage"])
		if !ok {
			s.fail(3)
			return
		}
		for _, state := range s.ids {
			if state == "todo" {
				s.fail(3)
			}
		}
		s.result.Usage = u
		s.terminal = true
	case "turn.failed":
		e, ok := m["error"].(map[string]any)
		if !s.turn || !keys(m, "type", "error") || !ok || !keys(e, "message") || !stringValue(e["message"]) {
			s.fail(3)
			return
		}
		s.terminal = true
		s.fail(1)
	case "error":
		if !s.thread || !keys(m, "type", "message") || !stringValue(m["message"]) {
			s.fail(3)
			return
		}
		s.fail(1)
	default:
		s.fail(3)
	}
}
func (s *codexStream) item(event string, m map[string]any) {
	id, ok := m["id"].(string)
	if !ok || id == "" {
		s.fail(3)
		return
	}
	typ, _ := m["type"].(string)
	if !s.turn && typ != "error" {
		s.fail(3)
		return
	}
	if s.ids == nil {
		s.ids = make(map[string]string)
	}
	old := s.ids[id]
	if typ != "todo_list" && old != "" && (old != typ || event == "item.started") {
		s.fail(3)
		return
	}
	switch typ {
	case "agent_message", "reasoning":
		if event != "item.completed" || !keys(m, "id", "type", "text") || !stringValue(m["text"]) {
			s.fail(3)
			return
		}
		text := m["text"].(string)
		if typ == "reasoning" {
			if strings.TrimSpace(text) == "" {
				s.fail(3)
				return
			}
		} else {
			s.answer = text
		}
		s.ids[id] = "closed"
	case "todo_list":
		if !keys(m, "id", "type", "items") {
			s.fail(3)
			return
		}
		items, ok := m["items"].([]any)
		if !ok {
			s.fail(3)
			return
		}
		for _, v := range items {
			x, ok := v.(map[string]any)
			if !ok || !keys(x, "text", "completed") || !stringValue(x["text"]) {
				s.fail(3)
				return
			}
			if _, ok := x["completed"].(bool); !ok {
				s.fail(3)
				return
			}
		}
		if event == "item.started" {
			if old != "" {
				s.fail(3)
				return
			}
			s.ids[id] = "todo"
		} else {
			if old != "todo" {
				s.fail(3)
				return
			}
			if event == "item.completed" {
				s.ids[id] = "closed"
			}
		}
	case "error":
		if event != "item.completed" || !keys(m, "id", "type", "message") || !stringValue(m["message"]) {
			s.fail(3)
			return
		}
		s.ids[id] = "closed"
		if s.turn {
			s.result.ErrorItemsInTurn++
		} else {
			s.result.ErrorItemsPreTurn++
		}
		if strings.HasPrefix(m["message"].(string), "model rerouted: ") {
			s.result.ModelReroutedSeen = true
		}
		s.fail(1)
	case "command_execution", "file_change", "mcp_tool_call", "collab_tool_call", "web_search":
		if !validAction(m) {
			s.fail(3)
			return
		}
		s.ids[id] = typ
		if event == "item.completed" {
			s.ids[id] = "closed"
		}
		s.fail(2)
	default:
		s.fail(3)
	}
}
func readUsage(v any) (usage, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return usage{}, false
	}
	_, optional := m["cache_write_input_tokens"]
	names := []string{"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_output_tokens"}
	if optional {
		names = append(names, "cache_write_input_tokens")
	}
	if !keys(m, names...) {
		return usage{}, false
	}
	var u usage
	for k, dst := range map[string]*int64{"input_tokens": &u.Input, "cached_input_tokens": &u.Cached, "output_tokens": &u.Output, "reasoning_output_tokens": &u.Reasoning, "cache_write_input_tokens": &u.CacheWrite} {
		v, present := m[k]
		if !present && k == "cache_write_input_tokens" {
			continue
		}
		n, ok := boundedInt(v)
		if !ok {
			return usage{}, false
		}
		*dst = n
	}
	return u, u.Cached <= u.Input && u.Reasoning <= u.Output
}
func (s *codexStream) finish(pending bool) streamResult {
	if pending {
		s.fail(3)
	}
	answer := sanitize(s.answer)
	if s.terminal && len(answer) > MaxAnswerBytes {
		s.fail(3)
	}
	r := s.result
	r.TurnStarted = s.turn
	r.Answer = ""
	switch s.rank {
	case 3:
		r.Verdict = "invalid_protocol"
	case 2:
		r.Verdict = "visible_action"
	case 1:
		r.Verdict = "vendor_error"
	default:
		if !s.terminal {
			r.Verdict = "terminal_missing"
			return r
		}
		valid := false
		for _, c := range s.answer {
			if c >= 32 && c != 127 && !unicode.IsSpace(c) {
				valid = true
				break
			}
		}
		if !valid {
			r.Verdict = "no_answer"
		} else {
			r.Verdict = "admitted"
			r.Answer = answer
		}
	}
	return r
}
func inspectCodex(data []byte) streamResult {
	var s codexStream
	if len(data) > streamCap {
		s.fail(3)
		return s.finish(false)
	}
	for {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			break
		}
		s.record(data[:nl])
		data = data[nl+1:]
	}
	return s.finish(len(data) > 0)
}

func enum(v any, values ...string) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func nullableString(v any) bool { return v == nil || stringValue(v) }
func stringArray(v any) bool {
	a, ok := v.([]any)
	if !ok {
		return false
	}
	for _, x := range a {
		if !stringValue(x) {
			return false
		}
	}
	return true
}
func validAction(m map[string]any) bool {
	switch m["type"] {
	case "command_execution":
		if !keys(m, "id", "type", "command", "aggregated_output", "exit_code", "status") || !stringValue(m["command"]) || !stringValue(m["aggregated_output"]) || !enum(m["status"], "in_progress", "completed", "failed", "declined") {
			return false
		}
		if m["exit_code"] != nil {
			n, ok := m["exit_code"].(json.Number)
			if !ok || strings.ContainsAny(string(n), ".eE") {
				return false
			}
			v, err := n.Int64()
			if err != nil || v < -2147483648 || v > 2147483647 {
				return false
			}
		}
	case "file_change":
		if !keys(m, "id", "type", "changes", "status") || !enum(m["status"], "in_progress", "completed", "failed") {
			return false
		}
		a, ok := m["changes"].([]any)
		if !ok {
			return false
		}
		for _, v := range a {
			x, ok := v.(map[string]any)
			if !ok || !keys(x, "path", "kind") || !stringValue(x["path"]) || !enum(x["kind"], "add", "delete", "update") {
				return false
			}
		}
	case "mcp_tool_call":
		if !keys(m, "id", "type", "server", "tool", "arguments", "result", "error", "status") || !stringValue(m["server"]) || !stringValue(m["tool"]) || !enum(m["status"], "in_progress", "completed", "failed") {
			return false
		}
		if m["error"] != nil {
			x, ok := m["error"].(map[string]any)
			if !ok || !keys(x, "message") || !stringValue(x["message"]) {
				return false
			}
		}
		if m["result"] != nil {
			x, ok := m["result"].(map[string]any)
			if !ok {
				return false
			}
			names := []string{"content", "structured_content"}
			if _, ok := x["_meta"]; ok {
				names = append(names, "_meta")
			}
			if !keys(x, names...) {
				return false
			}
			if _, ok := x["content"].([]any); !ok {
				return false
			}
		}
	case "collab_tool_call":
		if !keys(m, "id", "type", "tool", "sender_thread_id", "receiver_thread_ids", "prompt", "agents_states", "status") || !enum(m["tool"], "spawn_agent", "send_input", "wait", "close_agent") || !stringValue(m["sender_thread_id"]) || !stringArray(m["receiver_thread_ids"]) || !nullableString(m["prompt"]) || !enum(m["status"], "in_progress", "completed", "failed") {
			return false
		}
		states, ok := m["agents_states"].(map[string]any)
		if !ok {
			return false
		}
		for _, v := range states {
			x, ok := v.(map[string]any)
			if !ok || !keys(x, "status", "message") || !nullableString(x["message"]) || !enum(x["status"], "pending_init", "running", "interrupted", "completed", "errored", "shutdown", "not_found") {
				return false
			}
		}
	case "web_search":
		if !keys(m, "id", "type", "query", "action") || !stringValue(m["query"]) {
			return false
		}
		a, ok := m["action"].(map[string]any)
		if !ok {
			return false
		}
		allowed := []string{"type"}
		switch a["type"] {
		case "search":
			allowed = append(allowed, "query", "queries")
		case "open_page":
			allowed = append(allowed, "url")
		case "find_in_page":
			allowed = append(allowed, "url", "pattern")
		case "other":
		default:
			return false
		}
		for k, v := range a {
			found := false
			for _, name := range allowed {
				if k == name {
					found = true
				}
			}
			if !found {
				return false
			}
			if k == "queries" {
				if !stringArray(v) {
					return false
				}
			} else if !stringValue(v) {
				return false
			}
		}
	default:
		return false
	}
	return true
}
