package tools

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/signing"
)

const delegateProposalGoldenHMACKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

var (
	errDelegateProposalTestSigner   = errors.New("test signer failure")
	errDelegateProposalTestVerifier = errors.New("test verifier failure")
)

type delegateProposalErrorSigner struct{}

func (delegateProposalErrorSigner) KeyID() string                { return "test-key" }
func (delegateProposalErrorSigner) Algorithm() signing.Algorithm { return signing.AlgHMACSHA256 }
func (delegateProposalErrorSigner) Sign(context.Context, string, []byte) (signing.Signature, error) {
	return signing.Signature{}, errDelegateProposalTestSigner
}

type delegateProposalVerifierFunc func(context.Context, string, []byte, signing.Signature) error

func (f delegateProposalVerifierFunc) Verify(ctx context.Context, domain string, payload []byte, sig signing.Signature) error {
	return f(ctx, domain, payload, sig)
}

type delegateProposalMutatingVerifier struct{ err error }

func (v delegateProposalMutatingVerifier) Verify(_ context.Context, _ string, payload []byte, sig signing.Signature) error {
	payload[0] ^= 1
	sig.Bytes[0] ^= 1
	return v.err
}

func delegateProposalTestHMAC(t *testing.T) *signing.HMACSigner {
	t.Helper()
	key, err := hex.DecodeString(delegateProposalGoldenHMACKeyHex)
	if err != nil {
		t.Fatalf("decode fixed test HMAC key: %v", err)
	}
	signer, err := signing.NewHMAC(key)
	if err != nil {
		t.Fatalf("NewHMAC(fixed test key): %v", err)
	}
	return signer
}

func TestDelegateProposalGolden(t *testing.T) {
	t.Parallel()

	signer := delegateProposalTestHMAC(t)
	completionTime := time.Date(2026, 9, 5, 8, 0, 0, 987654321, time.FixedZone("EDT", -4*60*60))

	got, err := newDelegateProposal(
		context.Background(),
		signer,
		signer,
		"abc",
		"x",
		provider.ModelKey{Provider: "local", Model: "coder"},
		completionTime,
	)
	if err != nil {
		t.Fatalf("newDelegateProposal(golden inputs): %v", err)
	}
	if got == nil {
		t.Fatal("newDelegateProposal(golden inputs) = nil, want proposal")
	}

	canonicalBody, err := signing.MarshalCanonical(got.Body)
	if err != nil {
		t.Fatalf("MarshalCanonical(golden body): %v", err)
	}
	const wantBody = `{"content_form":"delegate-result/v1","content_sha256":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad","model":{"Model":"coder","Provider":"local"},"prompt_sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881","timestamp":"2026-09-05T12:00:00Z"}`
	if string(canonicalBody) != wantBody {
		t.Errorf("newDelegateProposal(golden inputs) canonical body = %s, want %s", canonicalBody, wantBody)
	}
	if got.Domain != "go-llm/delegate-proposal/v1" {
		t.Errorf("newDelegateProposal(golden inputs).Domain = %q, want %q", got.Domain, "go-llm/delegate-proposal/v1")
	}
	if got.Content != "abc" {
		t.Errorf("newDelegateProposal(golden inputs).Content = %q, want %q", got.Content, "abc")
	}
	const wantKeyID = "762993af3b094deb118ecd9d48f51724df4db52719309ff24845c54ff495e3bb"
	if got.Signature.Alg != signing.AlgHMACSHA256 || got.Signature.KeyID != wantKeyID {
		t.Errorf("newDelegateProposal(golden inputs).Signature binding = %q/%q, want %q/%q", got.Signature.Alg, got.Signature.KeyID, signing.AlgHMACSHA256, wantKeyID)
	}
	wantSignature, err := base64.StdEncoding.DecodeString("0YfhWMUnviYlfgeaFd2XiamHNYvrPQXl6i85kWFiGko=")
	if err != nil {
		t.Fatalf("decode literal golden signature: %v", err)
	}
	if !bytes.Equal(got.Signature.Bytes, wantSignature) {
		t.Errorf("newDelegateProposal(golden inputs).Signature.Bytes = %x, want %x", got.Signature.Bytes, wantSignature)
	}

	literalSignature := signing.Signature{
		Alg:   signing.AlgHMACSHA256,
		KeyID: wantKeyID,
		Bytes: wantSignature,
	}
	if err := signer.Verify(context.Background(), "go-llm/delegate-proposal/v1", []byte(wantBody), literalSignature); err != nil {
		t.Errorf("existing HMAC backend Verify(literal contract) = %v, want nil", err)
	}

	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal(golden proposal): %v", err)
	}
	var roundTrip DelegateProposal
	if err := json.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatalf("Unmarshal(trusted golden proposal): %v", err)
	}
	if err := VerifyDelegateProposal(context.Background(), signer, &roundTrip, "x"); err != nil {
		t.Errorf("VerifyDelegateProposal(trusted golden round trip) = %v, want nil", err)
	}
}

func TestDelegateProposalCreationRejectsInvalidInputsAndDependencyErrors(t *testing.T) {
	t.Parallel()

	validTime := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	validModel := provider.ModelKey{Provider: "local", Model: "coder"}
	validSigner := delegateProposalTestHMAC(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		signer   signing.Signer
		verifier DelegateProposalVerifier
		content  string
		prompt   string
		model    provider.ModelKey
		at       time.Time
		wantErr  error
	}{
		{name: "nil context", ctx: nil, signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: validModel, at: validTime},
		{name: "canceled context", ctx: canceled, signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: validModel, at: validTime, wantErr: context.Canceled},
		{name: "nil signer", ctx: context.Background(), verifier: validSigner, content: "abc", prompt: "x", model: validModel, at: validTime},
		{name: "nil verifier", ctx: context.Background(), signer: validSigner, content: "abc", prompt: "x", model: validModel, at: validTime},
		{name: "empty content", ctx: context.Background(), signer: validSigner, verifier: validSigner, prompt: "x", model: validModel, at: validTime},
		{name: "invalid UTF-8 content", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: string([]byte{0xff}), prompt: "x", model: validModel, at: validTime},
		{name: "oversize content", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: strings.Repeat("c", mutateMaxBytes+1), prompt: "x", model: validModel, at: validTime},
		{name: "empty prompt", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", model: validModel, at: validTime},
		{name: "blank prompt", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: " \t", model: validModel, at: validTime},
		{name: "invalid UTF-8 prompt", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: string([]byte{0xff}), model: validModel, at: validTime},
		{name: "empty provider", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: provider.ModelKey{Model: "coder"}, at: validTime},
		{name: "blank provider", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: provider.ModelKey{Provider: " \t", Model: "coder"}, at: validTime},
		{name: "empty model", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: provider.ModelKey{Provider: "local"}, at: validTime},
		{name: "blank model", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: provider.ModelKey{Provider: "local", Model: "\n"}, at: validTime},
		{name: "invalid UTF-8 provider", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: provider.ModelKey{Provider: string([]byte{0xff}), Model: "coder"}, at: validTime},
		{name: "invalid UTF-8 model", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: provider.ModelKey{Provider: "local", Model: string([]byte{0xff})}, at: validTime},
		{name: "oversize model identity", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: provider.ModelKey{Provider: strings.Repeat("p", 512), Model: strings.Repeat("m", 513)}, at: validTime},
		{name: "signer error", ctx: context.Background(), signer: delegateProposalErrorSigner{}, verifier: validSigner, content: "abc", prompt: "x", model: validModel, at: validTime, wantErr: errDelegateProposalTestSigner},
		{name: "verifier error", ctx: context.Background(), signer: validSigner, verifier: delegateProposalVerifierFunc(func(context.Context, string, []byte, signing.Signature) error { return errDelegateProposalTestVerifier }), content: "abc", prompt: "x", model: validModel, at: validTime, wantErr: errDelegateProposalTestVerifier},
		{name: "zero timestamp", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: validModel},
		{name: "unmarshalable timestamp", ctx: context.Background(), signer: validSigner, verifier: validSigner, content: "abc", prompt: "x", model: validModel, at: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := newDelegateProposal(tt.ctx, tt.signer, tt.verifier, tt.content, tt.prompt, tt.model, tt.at)
			if err == nil {
				t.Errorf("newDelegateProposal(%s) error = nil, want non-nil", tt.name)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("newDelegateProposal(%s) error = %v, want errors.Is(%v)", tt.name, err, tt.wantErr)
			}
			if got != nil {
				t.Errorf("newDelegateProposal(%s) = %#v, want nil on error", tt.name, got)
			}
		})
	}
}

func TestDelegateProposalCreationAcceptsExactLimits(t *testing.T) {
	t.Parallel()

	signer := delegateProposalTestHMAC(t)
	tests := []struct {
		name    string
		content string
		model   provider.ModelKey
	}{
		{name: "content", content: strings.Repeat("c", mutateMaxBytes), model: provider.ModelKey{Provider: "local", Model: "coder"}},
		{name: "model identity", content: "abc", model: provider.ModelKey{Provider: strings.Repeat("p", 512), Model: strings.Repeat("m", 512)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := newDelegateProposal(context.Background(), signer, signer, tt.content, "x", tt.model, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Errorf("newDelegateProposal(exact %s limit) error = %v, want nil", tt.name, err)
			}
			if got == nil {
				t.Errorf("newDelegateProposal(exact %s limit) = nil, want proposal", tt.name)
			}
		})
	}
}

func TestDelegateProposalVerifyBackends(t *testing.T) {
	t.Parallel()

	hmacSigner := delegateProposalTestHMAC(t)
	edSigner, err := signing.NewEd25519Signer(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("NewEd25519Signer(fixed test seed): %v", err)
	}
	keyring, err := signing.NewKeyring(hmacSigner)
	if err != nil {
		t.Fatalf("NewKeyring(delegate proposal verifier): %v", err)
	}
	tests := []struct {
		name     string
		signer   signing.Signer
		verifier DelegateProposalVerifier
	}{
		{name: "HMAC", signer: hmacSigner, verifier: hmacSigner},
		{name: "Ed25519", signer: edSigner, verifier: edSigner.Verifier()},
		{name: "purpose-scoped keyring", signer: hmacSigner, verifier: keyring},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			proposal := mustDelegateProposal(t, tt.signer, tt.verifier, "abc", "x")
			before := cloneDelegateProposal(proposal)
			if err := VerifyDelegateProposal(context.Background(), tt.verifier, proposal, "x"); err != nil {
				t.Errorf("VerifyDelegateProposal(%s) = %v, want nil", tt.name, err)
			}
			if !reflect.DeepEqual(proposal, before) {
				t.Errorf("VerifyDelegateProposal(%s) mutated proposal: got %#v, want %#v", tt.name, proposal, before)
			}
		})
	}
}

func TestDelegateProposalVerifyRejectsInvalidArgumentsAndKeys(t *testing.T) {
	t.Parallel()

	signer := delegateProposalTestHMAC(t)
	valid := mustDelegateProposal(t, signer, signer, "abc", "x")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	wrongKey, err := signing.NewHMAC(bytes.Repeat([]byte{0x99}, signing.MinHMACKeySize))
	if err != nil {
		t.Fatalf("NewHMAC(wrong test key): %v", err)
	}
	emptyKeyring := &signing.Keyring{}
	tests := []struct {
		name     string
		ctx      context.Context
		verifier DelegateProposalVerifier
		proposal *DelegateProposal
		wantErr  error
	}{
		{name: "nil context", verifier: signer, proposal: valid},
		{name: "nil verifier", ctx: context.Background(), proposal: valid},
		{name: "nil proposal", ctx: context.Background(), verifier: signer},
		{name: "canceled context", ctx: canceled, verifier: signer, proposal: valid, wantErr: context.Canceled},
		{name: "uninitialized key", ctx: context.Background(), verifier: &signing.HMACSigner{}, proposal: valid, wantErr: signing.ErrUninitializedKey},
		{name: "wrong key", ctx: context.Background(), verifier: wrongKey, proposal: valid, wantErr: signing.ErrKeyMismatch},
		{name: "unknown key", ctx: context.Background(), verifier: emptyKeyring, proposal: valid, wantErr: signing.ErrUnknownKey},
		{name: "wrapped verifier error", ctx: context.Background(), verifier: delegateProposalVerifierFunc(func(context.Context, string, []byte, signing.Signature) error {
			return fmt.Errorf("test wrapper: %w", errDelegateProposalTestVerifier)
		}), proposal: valid, wantErr: errDelegateProposalTestVerifier},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := cloneDelegateProposal(tt.proposal)
			err := VerifyDelegateProposal(tt.ctx, tt.verifier, tt.proposal, "x")
			if err == nil {
				t.Errorf("VerifyDelegateProposal(%s) = nil, want error", tt.name)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("VerifyDelegateProposal(%s) = %v, want errors.Is(%v)", tt.name, err, tt.wantErr)
			}
			if tt.proposal != nil && !reflect.DeepEqual(tt.proposal, before) {
				t.Errorf("VerifyDelegateProposal(%s) mutated proposal: got %#v, want %#v", tt.name, tt.proposal, before)
			}
		})
	}
}

func TestDelegateProposalVerifyProtectsInputsFromVerifierMutation(t *testing.T) {
	t.Parallel()

	signer := delegateProposalTestHMAC(t)
	tests := []struct {
		name    string
		wantErr error
	}{
		{name: "success"},
		{name: "failure", wantErr: errDelegateProposalTestVerifier},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			proposal := mustDelegateProposal(t, signer, signer, "abc", "x")
			before := cloneDelegateProposal(proposal)
			err := VerifyDelegateProposal(context.Background(), delegateProposalMutatingVerifier{err: tt.wantErr}, proposal, "x")
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("VerifyDelegateProposal(mutating verifier %s) = %v, want errors.Is(%v)", tt.name, err, tt.wantErr)
			}
			if !reflect.DeepEqual(proposal, before) {
				t.Errorf("VerifyDelegateProposal(mutating verifier %s) mutated proposal: got %#v, want %#v", tt.name, proposal, before)
			}
		})
	}
}

func TestDelegateProposalVerifyRejectsTampering(t *testing.T) {
	t.Parallel()

	signer := delegateProposalTestHMAC(t)
	valid := mustDelegateProposal(t, signer, signer, "abc", "x")
	tests := []struct {
		name   string
		prompt string
		mutate func(*DelegateProposal)
	}{
		{name: "retained content", prompt: "x", mutate: func(p *DelegateProposal) { p.Content = "abd" }},
		{name: "empty expected prompt", prompt: "", mutate: func(*DelegateProposal) {}},
		{name: "blank expected prompt", prompt: " \t", mutate: func(*DelegateProposal) {}},
		{name: "invalid UTF-8 expected prompt", prompt: string([]byte{0xff}), mutate: func(*DelegateProposal) {}},
		{name: "expected prompt", prompt: "y", mutate: func(*DelegateProposal) {}},
		{name: "whitespace-only prompt change", prompt: " x", mutate: func(*DelegateProposal) {}},
		{name: "domain", prompt: "x", mutate: func(p *DelegateProposal) { p.Domain = "go-llm/delegate-proposal/v2" }},
		{name: "content form", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.ContentForm = "delegate-result/v2" }},
		{name: "content digest", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.ContentSHA256 = strings.Repeat("0", 64) }},
		{name: "prompt digest", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.PromptSHA256 = strings.Repeat("0", 64) }},
		{name: "provider", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.Model.Provider = "other" }},
		{name: "model", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.Model.Model = "other" }},
		{name: "timestamp", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.Timestamp = p.Body.Timestamp.Add(time.Second) }},
		{name: "signature bytes", prompt: "x", mutate: func(p *DelegateProposal) { p.Signature.Bytes[0] ^= 1 }},
		{name: "signature key ID", prompt: "x", mutate: func(p *DelegateProposal) { p.Signature.KeyID = strings.Repeat("0", 64) }},
		{name: "signature algorithm", prompt: "x", mutate: func(p *DelegateProposal) { p.Signature.Alg = signing.AlgEd25519 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			proposal := cloneDelegateProposal(valid)
			tt.mutate(proposal)
			before := cloneDelegateProposal(proposal)
			if err := VerifyDelegateProposal(context.Background(), signer, proposal, tt.prompt); err == nil {
				t.Errorf("VerifyDelegateProposal(tampered %s) = nil, want error", tt.name)
			}
			if !reflect.DeepEqual(proposal, before) {
				t.Errorf("VerifyDelegateProposal(tampered %s) mutated proposal: got %#v, want %#v", tt.name, proposal, before)
			}
		})
	}
}

func TestDelegateProposalVerifyRejectsValidlySignedInvalidValues(t *testing.T) {
	t.Parallel()

	signer := delegateProposalTestHMAC(t)
	valid := mustDelegateProposal(t, signer, signer, "abc", "x")
	tests := []struct {
		name   string
		prompt string
		mutate func(*DelegateProposal)
	}{
		{name: "unsupported content form", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.ContentForm = "delegate-result/v2" }},
		{name: "nonzero timezone offset", prompt: "x", mutate: func(p *DelegateProposal) {
			p.Body.Timestamp = time.Date(2026, 9, 5, 8, 0, 0, 0, time.FixedZone("EDT", -4*60*60))
		}},
		{name: "subsecond timestamp", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.Timestamp = p.Body.Timestamp.Add(time.Nanosecond) }},
		{name: "zero timestamp", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.Timestamp = time.Time{} }},
		{name: "empty content", prompt: "x", mutate: func(p *DelegateProposal) { setDelegateProposalTestContent(p, "") }},
		{name: "invalid UTF-8 content", prompt: "x", mutate: func(p *DelegateProposal) { setDelegateProposalTestContent(p, string([]byte{0xff})) }},
		{name: "oversize content", prompt: "x", mutate: func(p *DelegateProposal) { setDelegateProposalTestContent(p, strings.Repeat("c", mutateMaxBytes+1)) }},
		{name: "blank model", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.Model.Model = " " }},
		{name: "oversize model", prompt: "x", mutate: func(p *DelegateProposal) {
			p.Body.Model = provider.ModelKey{Provider: strings.Repeat("p", 512), Model: strings.Repeat("m", 513)}
		}},
		{name: "uppercase content digest", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.ContentSHA256 = strings.ToUpper(p.Body.ContentSHA256) }},
		{name: "uppercase prompt digest", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.PromptSHA256 = strings.ToUpper(p.Body.PromptSHA256) }},
		{name: "different prompt", prompt: "x", mutate: func(p *DelegateProposal) { p.Body.PromptSHA256 = delegateProposalTestDigest("y") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			proposal := cloneDelegateProposal(valid)
			tt.mutate(proposal)
			signDelegateProposalTestBody(t, signer, proposal)
			before := cloneDelegateProposal(proposal)
			if err := VerifyDelegateProposal(context.Background(), signer, proposal, tt.prompt); err == nil {
				t.Errorf("VerifyDelegateProposal(validly signed %s) = nil, want error", tt.name)
			}
			if !reflect.DeepEqual(proposal, before) {
				t.Errorf("VerifyDelegateProposal(validly signed %s) mutated proposal: got %#v, want %#v", tt.name, proposal, before)
			}
		})
	}
}

func mustDelegateProposal(t *testing.T, signer signing.Signer, verifier DelegateProposalVerifier, content, prompt string) *DelegateProposal {
	t.Helper()
	proposal, err := newDelegateProposal(
		context.Background(),
		signer,
		verifier,
		content,
		prompt,
		provider.ModelKey{Provider: "local", Model: "coder"},
		time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("newDelegateProposal(test fixture): %v", err)
	}
	return proposal
}

func cloneDelegateProposal(proposal *DelegateProposal) *DelegateProposal {
	if proposal == nil {
		return nil
	}
	clone := *proposal
	clone.Signature.Bytes = bytes.Clone(proposal.Signature.Bytes)
	return &clone
}

func setDelegateProposalTestContent(proposal *DelegateProposal, content string) {
	proposal.Content = content
	proposal.Body.ContentSHA256 = delegateProposalTestDigest(content)
}

func delegateProposalTestDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func signDelegateProposalTestBody(t *testing.T, signer signing.Signer, proposal *DelegateProposal) {
	t.Helper()
	payload, err := signing.MarshalCanonical(proposal.Body)
	if err != nil {
		t.Fatalf("MarshalCanonical(validly signed invalid fixture): %v", err)
	}
	proposal.Signature, err = signer.Sign(context.Background(), "go-llm/delegate-proposal/v1", payload)
	if err != nil {
		t.Fatalf("Sign(validly signed invalid fixture): %v", err)
	}
}
