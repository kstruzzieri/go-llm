package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// readAgentflowState returns nil, nil for an absent file.
func readAgentflowState(dir, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, incompatibleAgentflowState(".agent/"+name, "unreadable")
	}
	return b, nil
}

func checkAgentflowStateVersion(where string, b []byte) error {
	var doc struct {
		SchemaVersion *string `json:"schema_version"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.SchemaVersion == nil {
		return incompatibleAgentflowState(where, "unreadable")
	}
	if !agentflow.SupportedSchemaVersion(*doc.SchemaVersion) {
		return incompatibleAgentflowState(where, fmt.Sprintf("schema_version %q", *doc.SchemaVersion))
	}
	return nil
}

func incompatibleAgentflowState(where, what string) error {
	return fmt.Errorf("this workspace holds incompatible AgentFlow state (%s %s); finish that run, or build its proof, with the AgentFlow version that wrote it, then move .agent/ aside before starting an AgentFlow 1.x run", where, what)
}
