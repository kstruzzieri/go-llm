package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/modeltext"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/signing"
)

// delegateTimeout bounds a single delegated code-generation sub-call. It is
// deliberately longer than the default tool timeout: a specialist model
// generating a whole file routinely exceeds the read-tool budget.
const delegateTimeout = 5 * time.Minute

// delegateSystemPrompt frames the specialist sub-call. The delegate returns a
// proposal; it must not claim disk writes or tool use.
const delegateSystemPrompt = "You are a code-generation specialist invoked for one scoped sub-task. " +
	"Return only the requested code or text, directly, with no preamble or explanation. " +
	"Do not claim to write files, run commands, or use tools; your output is a proposal the orchestrator will review and apply."

// DelegateCode routes one scoped code-generation sub-task to a specialist model
// (the caller is pinned to the configured delegate role chain) and returns the
// generated text as a NON-MUTATING tool result. It never touches disk: the
// orchestrator integrates the result through the approval-gated write tools.
type DelegateCode struct {
	caller             agent.ModelCaller
	onToken            func(string)
	proposalSigner     signing.Signer
	proposalVerifier   signing.Verifier
	proposalInitErr    error
	proposalConfigured bool
}

// ProposalVerifier returns the verifier for proposals emitted by this tool, or
// nil when proposal signing could not be initialized.
func (t *DelegateCode) ProposalVerifier() signing.Verifier {
	if t == nil || t.proposalInitErr != nil {
		return nil
	}
	return t.proposalVerifier
}

// DelegateOption configures optional DelegateCode behavior.
type DelegateOption func(*DelegateCode)

// WithStream streams the specialist's output tokens to sink as they arrive, so a
// caller can show progress during a long delegate call. A nil sink (or no option)
// leaves streaming off — the tool still returns the full result. The sink is
// display-only; it never affects the ToolResult.
func WithStream(sink func(string)) DelegateOption {
	return func(d *DelegateCode) { d.onToken = sink }
}

// WithProposalSigning configures the matching signer and verifier used for
// delegate proposals. Supplying either an incomplete or mismatched pair makes
// the tool unusable rather than falling back to a generated identity.
func WithProposalSigning(signer signing.Signer, verifier signing.Verifier) DelegateOption {
	return func(d *DelegateCode) {
		d.proposalConfigured = true
		if d.proposalInitErr != nil {
			return
		}
		if signer == nil || verifier == nil {
			d.proposalInitErr = errors.New("delegate proposal signing requires both signer and verifier")
			return
		}
		signerKeyID, verifierKeyID := signer.KeyID(), verifier.KeyID()
		signerAlgorithm, verifierAlgorithm := signer.Algorithm(), verifier.Algorithm()
		if signerKeyID == "" || verifierKeyID == "" || signerAlgorithm == "" || verifierAlgorithm == "" {
			d.proposalInitErr = errors.New("delegate proposal signing key is uninitialized")
			return
		}
		if signerKeyID != verifierKeyID || signerAlgorithm != verifierAlgorithm {
			d.proposalInitErr = errors.New("delegate proposal signer and verifier do not match")
			return
		}
		d.proposalSigner = signer
		d.proposalVerifier = verifier
	}
}

// NewDelegateCode builds the delegate_code tool over a caller pinned to the
// delegate role chain. Pass WithStream to surface generation progress.
func NewDelegateCode(caller agent.ModelCaller, opts ...DelegateOption) *DelegateCode {
	d := &DelegateCode{caller: caller}
	for _, o := range opts {
		o(d)
	}
	if d.proposalConfigured {
		return d
	}
	identity, err := signing.GenerateHMAC(nil)
	if err != nil {
		d.proposalInitErr = err
		return d
	}
	d.proposalSigner = identity
	d.proposalVerifier = identity
	return d
}

type delegateArgs struct {
	Prompt string `json:"prompt"`
}

func (*DelegateCode) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "delegate_code",
		Description: "Delegate one self-contained code-generation sub-task to a specialist coding model and return the generated code. " +
			"Use it for bulk or boilerplate generation, not for planning or decisions. The result is a proposal: review it before presenting or applying it. This tool does not modify any file.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "prompt":{"type":"string","description":"a precise, self-contained description of the code to generate"}
  },
  "required":["prompt"]
}`),
	}
}

// Effect is Read|Network (not plain Read): the tool makes an outbound model
// call, and Network keeps it off the read-only parallel dispatch path. The
// OutputCap matches the write tools' ceiling (mutateMaxBytes) so a large
// generated proposal is not truncated below what write_file/edit_file accept.
func (*DelegateCode) Effect() agent.Effect {
	return agent.Effect{
		Class:     agent.Read | agent.Network,
		Timeout:   delegateTimeout,
		OutputCap: mutateMaxBytes,
		Approval:  agent.ApprovalNever,
	}
}

// Origin declares delegate output model-authored (#436): another model's
// text, tagged like an assistant turn rather than blocked like foreign data.
func (*DelegateCode) Origin() agent.Origin { return agent.OriginModel }

func (t *DelegateCode) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	if t.proposalInitErr != nil {
		return agent.ToolResult{IsError: true, Content: "delegate failed: " + t.proposalInitErr.Error()}, nil
	}
	var args delegateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return agent.ToolResult{IsError: true, Content: "delegate_code requires a non-empty prompt"}, nil
	}

	req := provider.ChatRequest{
		Messages: []provider.ChatMessage{
			{Role: "system", Content: delegateSystemPrompt},
			{Role: "user", Content: args.Prompt},
		},
	}
	var onTok func(provider.ChatResponse) error
	if t.onToken != nil {
		onTok = func(chunk provider.ChatResponse) error {
			if chunk.Content != "" {
				t.onToken(chunk.Content)
			}
			return nil
		}
	}
	result, err := t.caller.Chat(ctx, req, onTok)
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "delegate failed: " + err.Error()}, nil
	}
	content := modeltext.StripCodeFence(strings.TrimSpace(result.Response.Content))
	if strings.TrimSpace(content) == "" {
		return agent.ToolResult{IsError: true, Content: "delegate returned no content"}, nil
	}
	if result.RouteOutcome == nil || strings.TrimSpace(result.RouteOutcome.ActualModel.Provider) == "" || strings.TrimSpace(result.RouteOutcome.ActualModel.Model) == "" {
		return agent.ToolResult{IsError: true, Content: "delegate failed: missing model routing identity"}, nil
	}
	model := result.RouteOutcome.ActualModel
	if !utf8.ValidString(model.Provider) || !utf8.ValidString(model.Model) || len(model.Provider)+len(model.Model) > delegateProposalModelMaxBytes {
		return agent.ToolResult{IsError: true, Content: "delegate failed: invalid model routing identity"}, nil
	}
	proposal, err := newDelegateProposal(ctx, t.proposalSigner, t.proposalVerifier, content, args.Prompt, model, time.Now())
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "delegate failed: " + err.Error()}, nil
	}
	provenance, err := json.Marshal(proposal)
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "delegate failed: " + err.Error()}, nil
	}
	return agent.ToolResult{
		Content:      content,
		Preview:      delegatePreview(result.RouteOutcome, content),
		RouteOutcome: result.RouteOutcome,
		Provenance:   provenance,
	}, nil
}

// delegatePreview renders the display-only summary line: the authoring model and
// output size. It never includes generated text.
func delegatePreview(outcome *provider.RouteOutcome, content string) string {
	model := "specialist model"
	if outcome != nil {
		if s := outcome.ActualModel.String(); s != "/" {
			model = s
		}
	}
	lines := strings.Count(content, "\n") + 1
	return fmt.Sprintf("delegated to %s · %d lines", model, lines)
}

// Wrapping-fence stripping lives in internal/modeltext: cmd/golem's source
// summarizer parses a JSON contract out of the same kind of local-model output
// and needs the identical rule, and two copies would drift on which shapes
// count as a wrapper.
