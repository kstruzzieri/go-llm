package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

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
// a concurrent writer; lock .agent/ if concurrent runs ever matter (relock
// policy is #643).
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
