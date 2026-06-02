package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
)

// claudeCLIProviderName is the judge provider instance identity for the
// claude-cli transport — folded into the cache key and provenance.
const claudeCLIProviderName = "claude-cli"

// claudeCLIModelAliases is the set of Claude Code model selectors the
// claude-cli transport validates against. Claude Code's aliases are stable;
// pass one of these via -judge-model (e.g. "opus"). This is an allowlist for
// fail-fast typo detection, not a placeholder — the CLI is the source of truth
// and would also reject an unknown --model at first call.
var claudeCLIModelAliases = []string{"opus", "sonnet", "haiku"}

// claudeCLIJudgeClient adapts the `claude` CLI (headless `-p` mode) to the
// Ollama-typed judge seams. It runs on the user's Claude subscription (no API
// billing) and is locked down for judging: the judge system prompt REPLACES
// Claude Code's default system prompt (--system-prompt), tools are disabled
// (--allowedTools ""), and each call runs in an empty working directory so no
// CLAUDE.md leaks into the judge context.
//
// Cleanliness caveat: this is the Claude Code *agent* in headless mode, not a
// bare chat completion. Even with the system-prompt override and tools
// disabled, the harness still injects its own scaffolding context (~tens of
// thousands of tokens of tool schemas / environment / reminders). Empirically
// the judge produces clean single-turn verdicts, but a citable calibration
// should treat this transport as "Claude Code judging", not "raw model
// judging", and ideally verify the lockdown rather than assume it.
//
// Determinism caveat: the CLI exposes no temperature flag, so the judge's
// configured temperature is NOT honored — the model runs at Claude Code's
// default. The categorical {0,0.5,1.0} rubric + off-grid repair re-prompt make
// this acceptable, but it is recorded here so provenance readers know the
// transport cannot pin temperature.
type claudeCLIJudgeClient struct {
	model string
	// run executes the claude CLI with args and stdin, returning stdout.
	// Injectable so unit tests drive the adapter without the real binary.
	run func(ctx context.Context, args []string, stdin string) ([]byte, error)
}

// newClaudeCLIJudge wraps the claude CLI as a judge transport for the given
// Claude Code model selector (e.g. "opus").
func newClaudeCLIJudge(model string) *claudeCLIJudgeClient {
	return &claudeCLIJudgeClient{model: strings.TrimSpace(model), run: runClaudeCLI}
}

// Chat implements judgeChatClient. The judge system prompt is passed as
// --system-prompt (replacing the default), the user prompt is fed on stdin,
// and the model's text (envelope .result) is returned as ChatResponse content
// for the unchanged judge parser to consume.
func (j *claudeCLIJudgeClient) Chat(ctx context.Context, req ollama.ChatRequest) (*ollama.ChatResponse, error) {
	system, user := splitJudgeMessages(req.Messages)
	args := []string{
		"-p",
		"--system-prompt", system,
		"--allowedTools", "",
		"--output-format", "json",
		"--model", j.model,
	}
	out, err := j.run(ctx, args, user)
	if err != nil {
		return nil, fmt.Errorf("claude-cli judge: %w", err)
	}
	result, err := claudeResultFromEnvelope(out)
	if err != nil {
		return nil, err
	}
	return &ollama.ChatResponse{
		Message: ollama.ChatMessage{Role: "assistant", Content: result},
		Done:    true,
	}, nil
}

// AvailableModels implements judgeModelChecker against the stable Claude Code
// alias allowlist so judge-model validation fails fast on a typo before any
// subscription call.
func (j *claudeCLIJudgeClient) AvailableModels(_ context.Context) ([]string, error) {
	out := make([]string, len(claudeCLIModelAliases))
	copy(out, claudeCLIModelAliases)
	return out, nil
}

// ShowModel implements judgeModelChecker by reporting the claude CLI version
// as the model "digest", so the CLI version pins the cached judge verdict (a
// CLI upgrade can change judge behavior). Errors degrade to (nil, nil) like
// the Ollama path, so a missing version never fails calibration.
func (j *claudeCLIJudgeClient) ShowModel(ctx context.Context, _ string) (*ollama.ModelInfo, error) {
	out, err := j.run(ctx, []string{"--version"}, "")
	if err != nil {
		return nil, nil
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return nil, nil
	}
	return &ollama.ModelInfo{Digest: "claude-cli/" + version}, nil
}

// splitJudgeMessages returns the system and user content from the judge's
// [system, user] message pair, tolerating either being absent.
func splitJudgeMessages(msgs []ollama.ChatMessage) (system, user string) {
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if system == "" {
				system = m.Content
			}
		case "user":
			if user == "" {
				user = m.Content
			}
		}
	}
	return system, user
}

// claudeResultFromEnvelope extracts the model's text from a `claude -p
// --output-format json` envelope. is_error envelopes are surfaced as errors.
func claudeResultFromEnvelope(out []byte) (string, error) {
	var env struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return "", fmt.Errorf("claude-cli judge: decode envelope: %w", err)
	}
	if env.IsError {
		return "", fmt.Errorf("claude-cli judge: claude reported is_error (subtype=%s)", env.Subtype)
	}
	return env.Result, nil
}

// runClaudeCLI is the production runner: it execs `claude` with args, feeds
// stdin, and runs in a fresh empty directory so no project CLAUDE.md leaks into
// the judge context. On non-zero exit it includes captured stderr.
func runClaudeCLI(ctx context.Context, args []string, stdin string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "judge-cli-")
	if err != nil {
		return nil, fmt.Errorf("mkdir empty judge cwd: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("exec claude: %w; stderr=%s", err, stderr)
	}
	return out, nil
}
