package interceptor

import (
	"context"
	"errors"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/secretscan"
)

const (
	// ConfiguredSecretsName is the interceptor name ConfiguredSecrets reports.
	ConfiguredSecretsName = "configured_secrets"
	// RuleConfiguredSecret is the rule of its one finding.
	RuleConfiguredSecret = "configured_secret"
)

var errNilConfiguredMatcher = errors.New("interceptor: configured secrets matcher must not be nil")

// ConfiguredSecrets blocks a tool call that carries a credential the host
// resolved from configuration (#424). It is exact where Secrets is
// shape-based: a credential of any shape matches, and a token-shaped string
// that is not configured does not.
//
// The matcher is the host's immutable lookup (config.Config.SecretMatcher).
// InspectOutput applies it to each call's ID, function name, and type, and to
// its arguments as raw text plus every decoded JSON object key and string
// value, before the response is recorded or published. Arguments that are not
// a JSON object or array (invalid JSON, or the string literal
// provider/openaicompat wraps such arguments in) are checked once more with
// their JSON escapes decoded leniently (secretscan.MatchToolArguments).
// InspectToolCall repeats the argument check before planning, approval, or
// invocation. Fields are never joined, so a credential split across two
// strings does not match.
//
// Each hook emits at most one finding, with the fixed Detail and no call
// identity: model-controlled IDs and names can hold the credential, so they
// never label their own finding. It also implements
// agent.SensitiveTextInterceptor, so a block drops the free text of every
// finding and error in that hook. It does not scan input, answers, thinking,
// or tool results; Secrets and Canary keep that coverage.
type ConfiguredSecrets struct {
	match func(string) bool
}

var _ agent.SensitiveTextInterceptor = ConfiguredSecrets{}

// NewConfiguredSecrets freezes match as the exact credential lookup. It errors
// on nil; pass a matcher that always reports false for an empty set.
func NewConfiguredSecrets(match func(string) bool) (ConfiguredSecrets, error) {
	if match == nil {
		return ConfiguredSecrets{}, errNilConfiguredMatcher
	}
	return ConfiguredSecrets{match: match}, nil
}

// Name returns "configured_secrets".
func (ConfiguredSecrets) Name() string { return ConfiguredSecretsName }

// InspectInput reports nothing: this guard covers tool calls only.
func (ConfiguredSecrets) InspectInput(context.Context, agent.InputInspection) ([]agent.Finding, error) {
	return nil, nil
}

// InspectOutput checks every collected call's ID, function name, type, and
// arguments, returning one finding for the batch on the first hit. A zero
// ConfiguredSecrets has no matcher and fails closed with an error.
func (c ConfiguredSecrets) InspectOutput(_ context.Context, out agent.OutputInspection) ([]agent.Finding, error) {
	if c.match == nil {
		return nil, errNilConfiguredMatcher
	}
	for _, call := range out.ToolCalls {
		if c.match(call.ID) || c.match(call.Function.Name) || c.match(call.Type) ||
			secretscan.MatchToolArguments(call.Function.Arguments, c.match) {
			return []agent.Finding{configuredFinding(agent.TargetOutputContent)}, nil
		}
	}
	return nil, nil
}

// InspectToolCall repeats the argument check before dispatch. It checks
// arguments only; InspectOutput checks tool call metadata.
func (c ConfiguredSecrets) InspectToolCall(_ context.Context, call agent.ToolCallInspection) ([]agent.Finding, error) {
	if c.match == nil {
		return nil, errNilConfiguredMatcher
	}
	if !secretscan.MatchToolArguments(call.Call.Function.Arguments, c.match) {
		return nil, nil
	}
	return []agent.Finding{configuredFinding(agent.TargetToolCall)}, nil
}

// SensitiveText reports whether text holds a configured credential. A zero
// ConfiguredSecrets holds none.
func (c ConfiguredSecrets) SensitiveText(text string) bool {
	return c.match != nil && c.match(text)
}

func configuredFinding(kind agent.TargetKind) agent.Finding {
	return agent.Finding{
		Rule: RuleConfiguredSecret, Verdict: agent.VerdictBlock, Risk: 100, Detail: "configured credential in tool call",
		Target: kind, StateIndex: -1, Group: -1, Alternative: -1,
	}
}
