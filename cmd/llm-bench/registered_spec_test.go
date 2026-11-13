package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// The two exec descriptions lose exactly the #777 sentence: the product text
// is the registered form plus a non-empty suffix that starts with the marker.
func TestRegisteredSpecOmitsTheCredentialSentence(t *testing.T) {
	const marker = " Commands that name a credential file"
	// The whole suffix registeredSpec drops: a sentence appended after it
	// later would be dropped too, so it must make this test red.
	const dropped = " Commands that name a credential file (.env and .env.* other than .env.example, .env.sample, .env.template and .env.dist; .netrc, _netrc, .npmrc, .pypirc, .git-credentials; anything under .ssh, .gnupg, .aws or .kube), or that use a credential directory or .git as dir, are refused, and the content of those files is withheld from output."
	for _, s := range []agent.ToolSpec{agenttools.NewRunCommand(nil, nil).Spec(), agenttools.NewStartCommand(nil, nil).Spec()} {
		got, err := registeredSpec(s)
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		suffix, ok := strings.CutPrefix(s.Description, got.Description)
		if !ok || suffix != dropped || strings.Contains(got.Description, marker) {
			t.Errorf("%s: registered form is not the product description minus exactly the credential sentence (suffix %d bytes)", s.Name, len(suffix))
		}
		if got.Name != s.Name || string(got.Parameters) != string(s.Parameters) {
			t.Errorf("%s: name or parameters changed", s.Name)
		}
	}
}

// Every other tool the corpus builders embed passes through unchanged.
func TestRegisteredSpecLeavesOtherToolsUnchanged(t *testing.T) {
	for _, s := range []agent.ToolSpec{
		agenttools.NewReadFile(nil).Spec(),
		agenttools.Retrieve{}.Spec(),
		agenttools.NewWriteFile(nil, nil).Spec(),
		agenttools.NewEditFile(nil, nil).Spec(),
		agenttools.NewCommandStatus(nil).Spec(),
		agenttools.NewCommandTail(nil).Spec(),
		agenttools.NewStopCommand(nil).Spec(),
		{Name: "mcp__docs__fetch", Description: "[mcp docs] Fetch a documentation page by URL.", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "other", Description: "Commands that name a credential file are a phrase here too. Commands that name a credential file"},
	} {
		got, err := registeredSpec(s)
		if err != nil || !reflect.DeepEqual(got, s) {
			t.Errorf("%s: changed or failed (error %v)", s.Name, err)
		}
	}
}

// A reworded or removed sentence is an error, never a silent pass-through.
func TestRegisteredSpecRequiresTheMarker(t *testing.T) {
	for _, name := range []string{"run_command", "start_command"} {
		if _, err := registeredSpec(agent.ToolSpec{Name: name, Description: "Run a command."}); err == nil {
			t.Errorf("%s without the marker: no error", name)
		}
	}
}
