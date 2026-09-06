package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/signing"
)

const (
	// DelegateProposalDomain separates v1 delegate proposal signatures from every other signed record.
	DelegateProposalDomain = "go-llm/delegate-proposal/v1"
	// DelegateProposalContentForm identifies the normalized delegate result bytes retained in Content.
	DelegateProposalContentForm   = "delegate-result/v1"
	delegateProposalModelMaxBytes = 1024
)

// DelegateProposal is an authenticated delegate result.
type DelegateProposal struct {
	Domain    string               `json:"domain"`
	Body      DelegateProposalBody `json:"body"`
	Content   string               `json:"content"`
	Signature signing.Signature    `json:"signature"`
}

// DelegateProposalBody is the complete signed portion of a delegate proposal.
type DelegateProposalBody struct {
	ContentForm   string            `json:"content_form"`
	ContentSHA256 string            `json:"content_sha256"`
	Model         provider.ModelKey `json:"model"`
	PromptSHA256  string            `json:"prompt_sha256"`
	Timestamp     time.Time         `json:"timestamp"`
}

// DelegateProposalVerifier verifies a delegate proposal signature.
type DelegateProposalVerifier interface {
	Verify(context.Context, string, []byte, signing.Signature) error
}

func newDelegateProposal(
	ctx context.Context,
	signer signing.Signer,
	verifier DelegateProposalVerifier,
	content string,
	prompt string,
	model provider.ModelKey,
	completionTime time.Time,
) (*DelegateProposal, error) {
	if ctx == nil {
		return nil, errors.New("delegate proposal: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("delegate proposal: context: %w", err)
	}
	if signer == nil {
		return nil, errors.New("delegate proposal: nil signer")
	}
	if verifier == nil {
		return nil, errors.New("delegate proposal: nil verifier")
	}
	timestamp := completionTime.UTC().Truncate(time.Second)
	if err := validateDelegateProposalFields(content, prompt, model, timestamp); err != nil {
		return nil, err
	}
	contentDigest := sha256.Sum256([]byte(content))
	promptDigest := sha256.Sum256([]byte(prompt))
	proposal := &DelegateProposal{
		Domain:  DelegateProposalDomain,
		Content: content,
		Body: DelegateProposalBody{
			ContentForm:   DelegateProposalContentForm,
			ContentSHA256: hex.EncodeToString(contentDigest[:]),
			Model:         model,
			PromptSHA256:  hex.EncodeToString(promptDigest[:]),
			Timestamp:     timestamp,
		},
	}
	payload, err := signing.MarshalCanonical(proposal.Body)
	if err != nil {
		return nil, fmt.Errorf("delegate proposal: canonicalize: %w", err)
	}
	proposal.Signature, err = signer.Sign(ctx, DelegateProposalDomain, payload)
	if err != nil {
		return nil, fmt.Errorf("delegate proposal: sign: %w", err)
	}
	if err := VerifyDelegateProposal(ctx, verifier, proposal, prompt); err != nil {
		return nil, fmt.Errorf("delegate proposal: verify created proposal: %w", err)
	}
	return proposal, nil
}

// VerifyDelegateProposal verifies the complete typed delegate proposal contract.
// It does not decode or authenticate arbitrary JSON bytes. A future untrusted
// reader must enforce exact schema and member spelling; reject duplicate and
// unknown fields, invalid Unicode, and trailing data; retain the compact raw
// body; and require its canonical bytes to equal signing.MarshalCanonical of
// the decoded body before calling this helper. Unknown-field rejection alone
// cannot detect every lossy JSON decode.
func VerifyDelegateProposal(ctx context.Context, verifier DelegateProposalVerifier, proposal *DelegateProposal, expectedPrompt string) error {
	if ctx == nil {
		return errors.New("delegate proposal: nil context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delegate proposal: context: %w", err)
	}
	if verifier == nil {
		return errors.New("delegate proposal: nil verifier")
	}
	if proposal == nil {
		return errors.New("delegate proposal: nil proposal")
	}
	if err := validateDelegateProposalValue(proposal, expectedPrompt); err != nil {
		return err
	}
	payload, err := signing.MarshalCanonical(proposal.Body)
	if err != nil {
		return fmt.Errorf("delegate proposal: canonicalize body: %w", err)
	}
	signature := proposal.Signature
	signature.Bytes = bytes.Clone(proposal.Signature.Bytes)
	if err := verifier.Verify(ctx, DelegateProposalDomain, payload, signature); err != nil {
		return fmt.Errorf("delegate proposal: verify signature: %w", err)
	}
	return nil
}

func validateDelegateProposalValue(proposal *DelegateProposal, expectedPrompt string) error {
	if proposal.Domain != DelegateProposalDomain {
		return errors.New("delegate proposal: invalid domain")
	}
	if proposal.Body.ContentForm != DelegateProposalContentForm {
		return errors.New("delegate proposal: unsupported content form")
	}
	if err := validateDelegateProposalFields(proposal.Content, expectedPrompt, proposal.Body.Model, proposal.Body.Timestamp); err != nil {
		return err
	}
	contentDigest := sha256.Sum256([]byte(proposal.Content))
	if proposal.Body.ContentSHA256 != hex.EncodeToString(contentDigest[:]) {
		return errors.New("delegate proposal: content digest mismatch")
	}
	promptDigest := sha256.Sum256([]byte(expectedPrompt))
	if proposal.Body.PromptSHA256 != hex.EncodeToString(promptDigest[:]) {
		return errors.New("delegate proposal: prompt digest mismatch")
	}
	return nil
}

func validateDelegateProposalFields(content, prompt string, model provider.ModelKey, timestamp time.Time) error {
	if content == "" || !utf8.ValidString(content) || len(content) > mutateMaxBytes {
		return errors.New("delegate proposal: invalid content")
	}
	if !utf8.ValidString(model.Provider) || !utf8.ValidString(model.Model) ||
		strings.TrimSpace(model.Provider) == "" || strings.TrimSpace(model.Model) == "" ||
		len(model.Provider)+len(model.Model) > delegateProposalModelMaxBytes {
		return errors.New("delegate proposal: invalid model identity")
	}
	if timestamp.IsZero() {
		return errors.New("delegate proposal: zero timestamp")
	}
	_, offset := timestamp.Zone()
	if offset != 0 || timestamp.Nanosecond() != 0 {
		return errors.New("delegate proposal: timestamp must be whole-second UTC")
	}
	if !utf8.ValidString(prompt) || strings.TrimSpace(prompt) == "" {
		return errors.New("delegate proposal: invalid expected prompt")
	}
	return nil
}
