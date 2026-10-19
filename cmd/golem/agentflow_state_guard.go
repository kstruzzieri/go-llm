package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"

	"github.com/kstruzzieri/go-llm/agentflow"
)

// agentflowStateDocs carry one schema_version each; every row of
// agentflowStateLedgers carries its own, and AgentFlow validates each row on
// read (#612 G16).
var (
	agentflowStateDocs    = []string{"plan.lock.json", "execution.contract.json"}
	agentflowStateLedgers = []string{"step-runs.jsonl", "command-receipts.jsonl", "file-receipts.jsonl", "verification-runs.jsonl"}
)

// checkAgentflowStateMajor refuses a workspace whose retained AgentFlow state
// was not written by AgentFlow 1.x, before any AgentFlow call can mutate it
// (#612 R5). Absent files and empty ledgers pass. The drift report and proof
// pack are not checked (finish regenerates the first; the second has its own
// historical rule). ponytail: a preflight for one operator, not a lock against
// a concurrent writer; lock .agent/ if concurrent runs ever matter.
func checkAgentflowStateMajor(root string) error {
	dir := filepath.Join(root, ".agent")
	for _, name := range agentflowStateDocs {
		b, err := readAgentflowState(dir, name)
		if err != nil {
			return err
		}
		if b == nil {
			continue
		}
		if err := checkAgentflowStateVersion(".agent/"+name, b); err != nil {
			return err
		}
	}
	for _, name := range agentflowStateLedgers {
		b, err := readAgentflowState(dir, name)
		if err != nil {
			return err
		}
		for i, line := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue // AgentFlow skips blank lines too
			}
			if err := checkAgentflowStateVersion(fmt.Sprintf(".agent/%s:%d", name, i+1), line); err != nil {
				return err
			}
		}
	}
	return nil
}

// agentflowExecutionHistory reports the first of AgentFlow's canonical
// execution artifacts (EXECUTION_ARTIFACT_PATHS) that a workspace retains: the
// execution contract, then any ledger with rows. "" means no history. It does
// not inspect attempts/, receipts/ or other ledgers, so a partial manual
// cleanup that keeps only those is not detected (#643 S1).
func agentflowExecutionHistory(root string) (string, error) {
	dir := filepath.Join(root, ".agent")
	contract, err := readAgentflowState(dir, agentflowStateDocs[1])
	if err != nil {
		return "", err
	}
	if contract != nil {
		return ".agent/" + agentflowStateDocs[1], nil
	}
	for _, name := range agentflowStateLedgers {
		b, err := readAgentflowState(dir, name)
		if err != nil {
			return "", err
		}
		rows := 0
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(line)) != 0 {
				rows++
			}
		}
		if rows > 0 {
			return fmt.Sprintf(".agent/%s has %d entries", name, rows), nil
		}
	}
	return "", nil
}

// refuseAgentflowExecutionHistory is the one check both fresh entries run, so
// task mode and -goal cannot drift (#643 S2, S4).
func refuseAgentflowExecutionHistory(root, prefix string) error {
	history, err := agentflowExecutionHistory(root)
	if err != nil {
		return err
	}
	if history == "" {
		return nil
	}
	return fmt.Errorf("%s: .agent/ holds AgentFlow execution history (%s); run -agentflow-status to see whether -agentflow-resume can continue it, or move .agent/ aside to start over", prefix, history)
}

// receiptIDPattern admits only AgentFlow-style receipt ids: CR<n>
// (receipts.py), or WT<source>-CR<n> after aggregate.py namespaces them. A
// cloned repo can carry .agent/ and main.go prints Golem's errors raw, so
// ledger text must not choose the words shown: spaces, combining marks and
// homoglyphs all pass a display-escape check.
var receiptIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// unrecoverableTimedOutReceipt returns the first command receipt root retains
// that AgentFlow counts as timed out (decision "timeout" or timed_out true,
// execution_coverage.py receipt_timed_out) and that no later attempt can
// supersede (_classify_timeout_receipt): root's step-run ledger does not record
// its attempt under its step, or that attempt's last event is completed. ""
// means none. AgentFlow 1.1 verifies a superseded receipt, and its next-action
// leads a blocked, failed, abandoned or still-open attempt to a replacement
// (agentflow#60, #70), so those pass. The id is the receipt's own when it
// matches receiptIDPattern, else the ledger line. Read-only: it never edits or
// deletes receipts.
func unrecoverableTimedOutReceipt(root string) (string, error) {
	const name = "command-receipts.jsonl"
	dir := filepath.Join(root, ".agent")
	b, err := readAgentflowState(dir, name)
	if err != nil {
		return "", err
	}
	type receipt struct{ id, step, attempt string }
	var timedOut []receipt
	for i, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue // AgentFlow skips blank lines too
		}
		where := fmt.Sprintf(".agent/%s:%d", name, i+1)
		value, err := parseAgentflowJSON(line)
		row, isObject := value.(agentflowJSONObject)
		if err != nil || !isObject {
			return "", incompatibleAgentflowState(where, "unreadable")
		}
		r, isTimeout := receipt{id: where}, false
		for _, member := range row { // the parser already folds duplicate keys, last wins
			s, isString := member.value.(agentflowJSONString)
			switch {
			case agentflowJSONStringEqualASCII(member.key, "id"):
				if isString && receiptIDPattern.MatchString(string(s)) {
					r.id = string(s)
				}
			case agentflowJSONStringEqualASCII(member.key, "step_id"):
				if isString {
					r.step = string(s)
				}
			case agentflowJSONStringEqualASCII(member.key, "attempt_id"):
				if isString {
					r.attempt = string(s)
				}
			case agentflowJSONStringEqualASCII(member.key, "decision"):
				if isString && agentflowJSONStringEqualASCII(s, "timeout") {
					isTimeout = true
				}
			case agentflowJSONStringEqualASCII(member.key, "timed_out"):
				if v, ok := member.value.(bool); ok && v {
					isTimeout = true
				}
			}
		}
		if isTimeout {
			timedOut = append(timedOut, r)
		}
	}
	if len(timedOut) == 0 {
		return "", nil // the step-run ledger is read only when it matters
	}
	rows, err := readStepRunRows(dir)
	if err != nil {
		return "", err
	}
	attempts := stepRunAttempts(rows)
	for _, r := range timedOut {
		a, known := attempts[r.attempt]
		if r.step == "" || !known || a.step != r.step || a.inconsistent {
			return r.id, nil
		}
		switch a.last {
		case "completed":
			return r.id, nil
		case "blocked", "failed", "abandoned", "claimed", "amendment_started", "in_progress", "verified":
		default:
			// A newer AgentFlow 1.x may add an event: fail closed.
			return "", fmt.Errorf("attempt %q has an event this Golem does not know (%q)", r.attempt, a.last)
		}
	}
	return "", nil
}

// stepRunRow is one row of a step-run ledger that names a string step_id and
// attempt_id; AgentFlow's reader skips the others. review marks an amendment
// linked to review findings (finding_refs, or reason_code review_feedback).
type stepRunRow struct {
	step, attempt, event string
	review               bool
}

// readStepRunRows reads dir's step-run ledger in order.
func readStepRunRows(dir string) ([]stepRunRow, error) {
	const name = "step-runs.jsonl"
	b, err := readAgentflowState(dir, name)
	if err != nil {
		return nil, err
	}
	var rows []stepRunRow
	for i, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue // AgentFlow skips blank lines too
		}
		value, err := parseAgentflowJSON(line)
		object, isObject := value.(agentflowJSONObject)
		if err != nil || !isObject {
			return nil, incompatibleAgentflowState(fmt.Sprintf(".agent/%s:%d", name, i+1), "unreadable")
		}
		var row stepRunRow
		var hasStep, hasAttempt bool
		for _, member := range object { // the parser already folds duplicate keys, last wins
			s, isString := member.value.(agentflowJSONString)
			switch {
			case agentflowJSONStringEqualASCII(member.key, "finding_refs"):
				refs, isArray := member.value.([]any)
				row.review = row.review || (isArray && len(refs) > 0)
			case !isString:
			case agentflowJSONStringEqualASCII(member.key, "step_id"):
				row.step, hasStep = string(s), true
			case agentflowJSONStringEqualASCII(member.key, "attempt_id"):
				row.attempt, hasAttempt = string(s), true
			case agentflowJSONStringEqualASCII(member.key, "event"):
				row.event = string(s)
			case agentflowJSONStringEqualASCII(member.key, "reason_code"):
				row.review = row.review || agentflowJSONStringEqualASCII(s, "review_feedback")
			}
		}
		if hasStep && hasAttempt {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// stepRunAttempt is one attempt as a step-run ledger records it: the step that
// first named it and its last event other than lease_renewed (AgentFlow
// execution.py LEASE_EVENTS). inconsistent marks an attempt id another step
// also names, or one closed twice (execution_coverage.py _ledger_attempts).
type stepRunAttempt struct {
	step, last   string
	inconsistent bool
}

func stepRunAttempts(rows []stepRunRow) map[string]stepRunAttempt {
	attempts := make(map[string]stepRunAttempt)
	closed := make(map[string]bool)
	for _, row := range rows {
		a, seen := attempts[row.attempt]
		if !seen {
			a.step = row.step
		} else if a.step != row.step {
			a.inconsistent = true
		}
		switch row.event {
		case "completed", "blocked", "failed", "abandoned": // execution.py TERMINAL_EVENTS
			a.inconsistent = a.inconsistent || closed[row.attempt]
			closed[row.attempt] = true
		}
		if row.event != "lease_renewed" {
			a.last = row.event
		}
		attempts[row.attempt] = a
	}
	return attempts
}

// unfinishedReviewAmendment reports whether root's step-run ledger records a
// review-linked amendment of step after the step's last completed event: work
// a replacement attempt opened with the step's own goal would drop.
func unfinishedReviewAmendment(root, step string) (bool, error) {
	rows, err := readStepRunRows(filepath.Join(root, ".agent"))
	if err != nil {
		return false, err
	}
	review := false
	for _, row := range rows {
		switch {
		case row.step != step:
		case row.event == "completed":
			review = false
		case row.event == "amendment_started" && row.review:
			review = true
		}
	}
	return review, nil
}

// refuseUnrecoverableTimeout reports a timed-out command receipt root retains
// that no attempt can supersede. AgentFlow's verify-run rejects it, so no
// recovery of that root reaches a proof (#642 A9). Golem never points at
// init-execution --force: that overwrites the contract and ledgers.
func refuseUnrecoverableTimeout(root string) error {
	id, err := unrecoverableTimedOutReceipt(root)
	if err != nil || id == "" {
		return err
	}
	return fmt.Errorf("retained timeout receipt %s in %s cannot be superseded by a later attempt, so this root's ledger cannot pass AgentFlow run verification; preserve the complete .agent/ directory by archiving it outside the working tree, then initialize and validate a new run; do not delete individual receipts", id, root)
}

// lockedPlanMatches reports whether .agent/plan.lock.json holds the same plan
// as planJSON, ignoring the lock fields. Any read or parse failure is a
// mismatch, so the caller's refusal stands.
func lockedPlanMatches(root string, planJSON []byte) bool {
	lock, err := readAgentflowState(filepath.Join(root, ".agent"), agentflowStateDocs[0])
	if err != nil || lock == nil {
		return false
	}
	lockDigest, err := canonicalPlanJSONSHA256(lock)
	if err != nil {
		return false
	}
	planDigest, err := canonicalPlanJSONSHA256(planJSON)
	return err == nil && planDigest == lockDigest
}

// readAgentflowState reads only regular files and returns nil, nil for an absent
// file. The Unix open is nonblocking so a FIFO cannot stall the guard before
// the opened file's type is checked.
func readAgentflowState(dir, name string) ([]byte, error) {
	f, err := openAgentflowStateFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, incompatibleAgentflowState(".agent/"+name, "unreadable")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, incompatibleAgentflowState(".agent/"+name, "unreadable")
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, incompatibleAgentflowState(".agent/"+name, "unreadable")
	}
	return b, nil
}

// checkAgentflowStateVersion reads only the top-level schema_version. AgentFlow
// is Python and persists an unknown non-finite number as NaN/Infinity, which
// encoding/json rejects, so the document goes through the Python-dialect parser
// the plan binding already uses.
func checkAgentflowStateVersion(where string, b []byte) error {
	version, ok := agentflowStateSchemaVersion(b)
	if !ok {
		return incompatibleAgentflowState(where, "unreadable")
	}
	if !agentflow.SupportedSchemaVersion(version) {
		return incompatibleAgentflowState(where, fmt.Sprintf("schema_version %q", version))
	}
	return nil
}

func agentflowStateSchemaVersion(b []byte) (string, bool) {
	value, err := parseAgentflowJSON(b)
	doc, isObject := value.(agentflowJSONObject)
	if err != nil || !isObject {
		return "", false
	}
	for _, member := range doc { // the parser already folds duplicate keys, last wins
		if agentflowJSONStringEqualASCII(member.key, "schema_version") {
			version, isString := member.value.(agentflowJSONString)
			return string(version), isString
		}
	}
	return "", false
}

func incompatibleAgentflowState(where, what string) error {
	return fmt.Errorf("this workspace holds incompatible AgentFlow state (%s %s); finish that run, or build its proof, with the AgentFlow version that wrote it, then move .agent/ aside before starting an AgentFlow 1.x run", where, what)
}
