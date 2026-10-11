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

// retainedTimedOutReceipt returns the first command receipt root retains that
// AgentFlow 1.0 counts as timed out (decision "timeout" or timed_out true,
// execution_coverage.py _receipt_timed_out), or "". The id is the receipt's
// own when it matches receiptIDPattern, else the ledger line. Read-only: it
// never edits or deletes receipts.
func retainedTimedOutReceipt(root string) (string, error) {
	const name = "command-receipts.jsonl"
	b, err := readAgentflowState(filepath.Join(root, ".agent"), name)
	if err != nil {
		return "", err
	}
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
		id, timedOut := where, false
		for _, member := range row { // the parser already folds duplicate keys, last wins
			switch {
			case agentflowJSONStringEqualASCII(member.key, "id"):
				if s, ok := member.value.(agentflowJSONString); ok && receiptIDPattern.MatchString(string(s)) {
					id = string(s)
				}
			case agentflowJSONStringEqualASCII(member.key, "decision"):
				if s, ok := member.value.(agentflowJSONString); ok && agentflowJSONStringEqualASCII(s, "timeout") {
					timedOut = true
				}
			case agentflowJSONStringEqualASCII(member.key, "timed_out"):
				if v, ok := member.value.(bool); ok && v {
					timedOut = true
				}
			}
		}
		if timedOut {
			return id, nil
		}
	}
	return "", nil
}

// refuseRetainedTimeout reports a timed-out command receipt that root retains.
// AgentFlow 1.0's verify-run rejects every such receipt, superseded or not
// (agentflow#60), so no recovery of that root reaches a proof (#642 A9). Golem
// never points at init-execution --force: that overwrites the contract and
// ledgers.
func refuseRetainedTimeout(root string) error {
	id, err := retainedTimedOutReceipt(root)
	if err != nil || id == "" {
		return err
	}
	return retainedTimeoutReport(root, id)
}

// retainedTimeoutReport is the one text of the A9 report.
func retainedTimeoutReport(root, id string) error {
	return fmt.Errorf("retained timeout receipt %s in %s prevents this root's ledger from passing AgentFlow 1.0 run verification; preserve the complete .agent/ directory by archiving it outside the working tree, then initialize and validate a new run; do not delete individual receipts", id, root)
}

// closedAttemptEvent returns the first terminal event (AgentFlow 1.0
// execution.py TERMINAL_EVENTS: completed, blocked, failed, abandoned) that
// root's step-run ledger records for step's attempt, or "". AgentFlow's
// block-step checks only that the attempt was opened, and its ledger reader
// rejects any event after a terminal one ("attempt ... has events after a
// terminal event", porcelain.py) as state_invalid. An event outside AgentFlow
// 1.0's terminal, open and lease sets is an error, so the caller does not
// block. Read-only.
func closedAttemptEvent(root, step, attempt string) (string, error) {
	const name = "step-runs.jsonl"
	b, err := readAgentflowState(filepath.Join(root, ".agent"), name)
	if err != nil {
		return "", err
	}
	for i, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue // AgentFlow skips blank lines too
		}
		value, err := parseAgentflowJSON(line)
		row, isObject := value.(agentflowJSONObject)
		if err != nil || !isObject {
			return "", incompatibleAgentflowState(fmt.Sprintf(".agent/%s:%d", name, i+1), "unreadable")
		}
		var ofStep, ofAttempt bool
		var event string
		for _, member := range row { // the parser already folds duplicate keys, last wins
			s, isString := member.value.(agentflowJSONString)
			switch {
			case !isString:
			case agentflowJSONStringEqualASCII(member.key, "step_id"):
				ofStep = string(s) == step
			case agentflowJSONStringEqualASCII(member.key, "attempt_id"):
				ofAttempt = string(s) == attempt
			case agentflowJSONStringEqualASCII(member.key, "event"):
				event = string(s)
			}
		}
		if ofStep && ofAttempt {
			switch event {
			case "completed", "blocked", "failed", "abandoned":
				return event, nil
			case "claimed", "amendment_started", "in_progress", "verified", "lease_renewed":
			default:
				// A newer AgentFlow 1.x may add a terminal event: fail closed.
				return "", fmt.Errorf("attempt %s has an event this Golem does not know (%q)", attempt, event)
			}
		}
	}
	return "", nil
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
