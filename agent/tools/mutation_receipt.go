package tools

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/signing"
)

// MutationReceiptDomain separates v1 mutation receipts from other signed records.
const MutationReceiptDomain = "go-llm/mutation-receipt/v1"

// MaxMutationReceiptBytes is the 32 KiB envelope limit checked by signing,
// decoding, and verification before canonicalization and its allocations.
const MaxMutationReceiptBytes = 32 * 1024

// MutationReceiptBody is the complete signed intent or applied mutation record.
// MutationID is allocated by the host with crypto/rand.Text inside Prepare,
// before persistence or filesystem mutation. An applied body changes only Kind
// and Timestamp from its intent. WorkspaceHash
// is ContentHash of the canonical absolute workspace root. Path is the canonical
// slash-separated workspace-relative spelling from CanonicalPathForUndo.
// BeforeHash and AfterHash are ContentHash values or "absent", distinct from an
// empty file's hash. Timestamp is canonical UTC RFC3339Nano, an observation of
// the local clock rather than trusted time. AgentID names the signing key.
// AfterMode is non-nil only for forward tracked creates, including mode 0000;
// overwrites, untracked creates, and all inverses use nil (explicit JSON null).
// UndoOf is empty for a forward mutation or the original MutationID for an undo.
// Prior content and mutable journal bookkeeping are outside this signed body.
type MutationReceiptBody struct {
	Kind          string  `json:"kind"`
	MutationID    string  `json:"mutation_id"`
	WorkspaceHash string  `json:"workspace_hash"`
	Path          string  `json:"path"`
	BeforeHash    string  `json:"before_hash"`
	AfterHash     string  `json:"after_hash"`
	Timestamp     string  `json:"timestamp"`
	AgentID       string  `json:"agent_id"`
	AfterMode     *uint32 `json:"after_mode"`
	UndoOf        string  `json:"undo_of"`
}

// MutationReceipt is a portable body with its detached signature.
type MutationReceipt struct {
	Body      MutationReceiptBody `json:"body"`
	Signature signing.Signature   `json:"signature"`
}

// SignMutationReceipt validates and signs the complete body under
// MutationReceiptDomain. Body.AgentID must already equal signer.KeyID(). The
// encoded envelope must fit MaxMutationReceiptBytes.
func SignMutationReceipt(ctx context.Context, signer signing.Signer, body MutationReceiptBody) (MutationReceipt, error) {
	if err := mutationReceiptContext(ctx); err != nil {
		return MutationReceipt{}, err
	}
	if signer == nil {
		return MutationReceipt{}, signing.ErrUninitializedKey
	}
	if err := validateMutationReceiptBody(body); err != nil {
		return MutationReceipt{}, err
	}
	if body.AgentID != signer.KeyID() {
		return MutationReceipt{}, signing.ErrKeyMismatch
	}
	if body.AfterMode != nil {
		mode := *body.AfterMode
		body.AfterMode = &mode
	}
	// Reserve the exact wire size of the backend's signature before invoking
	// the canonicalizer. Successful signatures must have this binding and size.
	size := ed25519.SignatureSize
	if signer.Algorithm() == signing.AlgHMACSHA256 {
		size = sha256.Size
	}
	receipt := MutationReceipt{Body: body, Signature: signing.Signature{
		Alg: signer.Algorithm(), KeyID: signer.KeyID(), Bytes: make([]byte, size),
	}}
	if err := validateMutationReceiptSignature(receipt); err != nil {
		return MutationReceipt{}, err
	}
	if err := mutationReceiptSize(receipt); err != nil {
		return MutationReceipt{}, err
	}
	payload, err := signing.MarshalCanonical(body)
	if err != nil {
		return MutationReceipt{}, err
	}
	sig, err := signer.Sign(ctx, MutationReceiptDomain, payload)
	if err != nil {
		return MutationReceipt{}, fmt.Errorf("tools: sign mutation receipt: %w", err)
	}
	if sig.Alg != signer.Algorithm() || sig.KeyID != signer.KeyID() {
		return MutationReceipt{}, signing.ErrKeyMismatch
	}
	receipt.Signature = sig
	if err := validateMutationReceiptSignature(receipt); err != nil {
		return MutationReceipt{}, err
	}
	return receipt, nil
}

// DecodeMutationReceipt strictly decodes an envelope of at most
// MaxMutationReceiptBytes. Equivalent whitespace and member order are accepted;
// duplicate, unknown, missing, case-variant, null, or mistyped members are not
// (AfterMode alone permits null). It validates semantics and signature shape,
// but callers must use VerifyMutationReceipt with a trusted verifier before
// relying on the record. Ordinary json.Unmarshal does not enforce this contract.
func DecodeMutationReceipt(raw []byte) (MutationReceipt, error) {
	if len(raw) > MaxMutationReceiptBytes {
		return MutationReceipt{}, fmt.Errorf("tools: mutation receipt exceeds %d bytes", MaxMutationReceiptBytes)
	}
	canonical, err := signing.Canonicalize(raw)
	if err != nil {
		return MutationReceipt{}, fmt.Errorf("tools: decode mutation receipt: %w", err)
	}
	envelope, err := mutationReceiptObject(canonical, "body signature")
	if err != nil {
		return MutationReceipt{}, err
	}
	if _, err := mutationReceiptObject(envelope["body"], "kind mutation_id workspace_hash path before_hash after_hash timestamp agent_id after_mode undo_of"); err != nil {
		return MutationReceipt{}, fmt.Errorf("tools: mutation receipt body: %w", err)
	}
	if _, err := mutationReceiptObject(envelope["signature"], "alg kid sig"); err != nil {
		return MutationReceipt{}, fmt.Errorf("tools: mutation receipt signature: %w", err)
	}
	var receipt MutationReceipt
	if err := json.Unmarshal(canonical, &receipt); err != nil {
		return MutationReceipt{}, fmt.Errorf("tools: decode mutation receipt: %w", err)
	}
	if err := validateMutationReceiptBody(receipt.Body); err != nil {
		return MutationReceipt{}, err
	}
	if err := validateMutationReceiptSignature(receipt); err != nil {
		return MutationReceipt{}, err
	}
	return receipt, nil
}

// VerifyMutationReceipt authenticates the complete body and validates its
// semantics and key/algorithm binding. The caller supplies a trusted verifier;
// signature validity alone does not authorize a filesystem operation, prove
// intent/applied pairing, or detect replay, deletion, reordering, or rollback.
func VerifyMutationReceipt(ctx context.Context, verifier signing.Verifier, receipt MutationReceipt) error {
	if err := mutationReceiptContext(ctx); err != nil {
		return err
	}
	if verifier == nil {
		return signing.ErrUninitializedKey
	}
	if err := validateMutationReceiptBody(receipt.Body); err != nil {
		return err
	}
	if err := validateMutationReceiptSignature(receipt); err != nil {
		return err
	}
	if receipt.Signature.KeyID != verifier.KeyID() || receipt.Signature.Alg != verifier.Algorithm() {
		return signing.ErrKeyMismatch
	}
	if err := mutationReceiptSize(receipt); err != nil {
		return err
	}
	payload, err := signing.MarshalCanonical(receipt.Body)
	if err != nil {
		return err
	}
	if err := verifier.Verify(ctx, MutationReceiptDomain, payload, receipt.Signature); err != nil {
		return fmt.Errorf("tools: verify mutation receipt: %w", err)
	}
	return nil
}

func mutationReceiptSize(receipt MutationReceipt) error {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false) // Match the canonicalizer's string spelling.
	if err := enc.Encode(receipt); err != nil {
		return fmt.Errorf("tools: encode mutation receipt: %w", err)
	}
	if raw.Len()-1 > MaxMutationReceiptBytes { // Encoder appends one newline.
		return fmt.Errorf("tools: mutation receipt exceeds %d bytes", MaxMutationReceiptBytes)
	}
	return nil
}

func mutationReceiptContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("tools: mutation receipt: nil context")
	}
	return ctx.Err()
}

// Lexical validity and duplicate decoded names have already been checked by
// Canonicalize. Check exact membership and null before typed decoding can turn
// missing/null strings into zero values or match names case-insensitively.
func mutationReceiptObject(raw []byte, members string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	names := strings.Fields(members)
	if len(object) != len(names) {
		return nil, errors.New("tools: mutation receipt: wrong object members")
	}
	for _, name := range names {
		value, ok := object[name]
		if !ok || name != "after_mode" && bytes.Equal(value, []byte("null")) {
			return nil, fmt.Errorf("tools: mutation receipt: missing or null %s", name)
		}
	}
	return object, nil
}

func validateMutationReceiptBody(body MutationReceiptBody) error {
	if body.Kind != "intent" && body.Kind != "applied" {
		return errors.New("tools: mutation receipt: invalid kind")
	}
	if !validMutationID(body.MutationID) || body.UndoOf != "" && (!validMutationID(body.UndoOf) || body.UndoOf == body.MutationID) {
		return errors.New("tools: mutation receipt: invalid mutation identity")
	}
	if !mutationReceiptHash(body.WorkspaceHash) || !mutationReceiptHash(body.AgentID) {
		return errors.New("tools: mutation receipt: invalid workspace or agent hash")
	}
	if body.BeforeHash != absentHash && !mutationReceiptHash(body.BeforeHash) || body.AfterHash != absentHash && !mutationReceiptHash(body.AfterHash) {
		return errors.New("tools: mutation receipt: invalid content hash")
	}
	if body.BeforeHash == absentHash && body.AfterHash == absentHash {
		return errors.New("tools: mutation receipt: both contents are absent")
	}
	if !fs.ValidPath(body.Path) || body.Path == "." || strings.ContainsAny(body.Path, "\\\x00") || len(body.Path) >= 2 && body.Path[1] == ':' {
		return errors.New("tools: mutation receipt: noncanonical relative path")
	}
	at, err := time.Parse(time.RFC3339Nano, body.Timestamp)
	if err != nil || at.UTC().Format(time.RFC3339Nano) != body.Timestamp {
		return errors.New("tools: mutation receipt: noncanonical UTC timestamp")
	}
	if body.AfterMode != nil && (*body.AfterMode > 0o777 || body.BeforeHash != absentHash || body.AfterHash == absentHash || body.UndoOf != "") {
		return errors.New("tools: mutation receipt: mode requires a forward create and permission bits only")
	}
	return nil
}

func mutationReceiptHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validMutationID(value string) bool {
	// rand.Text promises at least 128 bits and may increase its output length.
	if len(value) < 26 {
		return false
	}
	for _, c := range value {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

func validateMutationReceiptSignature(receipt MutationReceipt) error {
	sig := receipt.Signature
	if sig.KeyID != receipt.Body.AgentID {
		return signing.ErrKeyMismatch
	}
	var size int
	switch sig.Alg {
	case signing.AlgEd25519:
		size = ed25519.SignatureSize
	case signing.AlgHMACSHA256:
		size = sha256.Size
	default:
		return errors.New("tools: mutation receipt: unsupported signature algorithm")
	}
	if len(sig.Bytes) != size {
		return signing.ErrInvalidSignature
	}
	return nil
}
