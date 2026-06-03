package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func argHasValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func judgeCLIReqFixture() ollama.ChatRequest {
	return ollama.ChatRequest{
		Model: "opus",
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: judgeSystemPrompt},
			{Role: "user", Content: "USER-PROMPT-MARKER"},
		},
		Format:  "json",
		Options: &ollama.ModelOptions{Temperature: 0.1, NumPredict: 512},
	}
}

func TestClaudeCLIJudge_ChatExecsLockedDownAndReturnsResult(t *testing.T) {
	var gotArgs []string
	var gotStdin string
	j := &claudeCLIJudgeClient{model: "opus", run: func(_ context.Context, args []string, stdin string) ([]byte, error) {
		gotArgs, gotStdin = args, stdin
		return []byte(`{"type":"result","is_error":false,"result":"{\"answer_quality\":1.0,\"justification\":\"good\"}"}`), nil
	}}

	resp, err := j.Chat(context.Background(), judgeCLIReqFixture())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp == nil || resp.Message.Content != `{"answer_quality":1.0,"justification":"good"}` {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if gotStdin != "USER-PROMPT-MARKER" {
		t.Fatalf("stdin = %q; want the judge user prompt", gotStdin)
	}
	if !argsContain(gotArgs, "-p") {
		t.Fatalf("missing -p (headless): %v", gotArgs)
	}
	if !argHasValue(gotArgs, "--system-prompt", judgeSystemPrompt) {
		t.Fatalf("--system-prompt not set to judge rubric: %v", gotArgs)
	}
	if !argHasValue(gotArgs, "--allowedTools", "") {
		t.Fatalf("--allowedTools not disabled: %v", gotArgs)
	}
	if !argHasValue(gotArgs, "--tools", "") {
		t.Fatalf("--tools not disabled: %v", gotArgs)
	}
	if !argHasValue(gotArgs, "--output-format", "json") {
		t.Fatalf("--output-format not json: %v", gotArgs)
	}
	if !argHasValue(gotArgs, "--model", "opus") {
		t.Fatalf("--model not opus: %v", gotArgs)
	}
}

func TestClaudeCLIJudge_ChatSurfacesEnvelopeError(t *testing.T) {
	j := &claudeCLIJudgeClient{model: "opus", run: func(_ context.Context, _ []string, _ string) ([]byte, error) {
		return []byte(`{"type":"result","is_error":true,"subtype":"error_max_turns","result":""}`), nil
	}}
	if _, err := j.Chat(context.Background(), judgeCLIReqFixture()); err == nil {
		t.Fatalf("Chat error = nil; want is_error envelope surfaced")
	}
}

func TestClaudeCLIJudge_ChatSurfacesRunnerError(t *testing.T) {
	j := &claudeCLIJudgeClient{model: "opus", run: func(_ context.Context, _ []string, _ string) ([]byte, error) {
		return nil, errors.New("claude not found")
	}}
	if _, err := j.Chat(context.Background(), judgeCLIReqFixture()); err == nil {
		t.Fatalf("Chat error = nil; want runner error surfaced")
	}
}

func TestClaudeCLIJudge_AvailableModelsIncludesAliases(t *testing.T) {
	j := &claudeCLIJudgeClient{model: "opus"}
	models, err := j.AvailableModels(context.Background())
	if err != nil {
		t.Fatalf("AvailableModels: %v", err)
	}
	if !argsContain(models, "opus") {
		t.Fatalf("models = %v; want to include opus", models)
	}
}

func TestClaudeCLIJudge_ShowModelReportsCLIVersionAsDigest(t *testing.T) {
	j := &claudeCLIJudgeClient{model: "opus", run: func(_ context.Context, args []string, _ string) ([]byte, error) {
		if !argsContain(args, "--version") {
			t.Errorf("ShowModel did not invoke --version: %v", args)
		}
		return []byte("2.1.159 (Claude Code)\n"), nil
	}}
	info, err := j.ShowModel(context.Background(), "opus")
	if err != nil {
		t.Fatalf("ShowModel: %v", err)
	}
	if info == nil || !strings.Contains(info.Digest, "2.1.159") {
		t.Fatalf("info = %+v; want digest carrying the CLI version", info)
	}
}

func TestClaudeCLIJudge_ShowModelDegradesOnRunnerError(t *testing.T) {
	j := &claudeCLIJudgeClient{model: "opus", run: func(_ context.Context, _ []string, _ string) ([]byte, error) {
		return nil, errors.New("no claude binary")
	}}
	info, err := j.ShowModel(context.Background(), "opus")
	if err != nil || info != nil {
		t.Fatalf("ShowModel on runner error = (%+v, %v); want (nil, nil) degrade", info, err)
	}
}

func TestNewJudgeTransport_ClaudeCLI(t *testing.T) {
	tr, err := newJudgeTransport(scorerOptions{judgeTransport: "claude-cli", judgeModel: "opus"})
	if err != nil {
		t.Fatalf("newJudgeTransport: %v", err)
	}
	if tr.providerName != claudeCLIProviderName {
		t.Fatalf("providerName = %q; want %q", tr.providerName, claudeCLIProviderName)
	}
	if _, ok := tr.chat.(*claudeCLIJudgeClient); !ok {
		t.Fatalf("chat is %T; want *claudeCLIJudgeClient", tr.chat)
	}
	if _, ok := tr.checker.(*claudeCLIJudgeClient); !ok {
		t.Fatalf("checker is %T; want *claudeCLIJudgeClient", tr.checker)
	}
}
