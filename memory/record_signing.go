package memory

import (
	"context"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/signing"
)

var (
	errMissingRecordSignature = errors.New("memory: missing record signature")
	errInvalidRecordBody      = errors.New("memory: invalid record body")
	errInvalidRecordTrust     = errors.New("memory: unknown record trust class")
)

// normalizeRecordTimes matches SQLite's millisecond round trip, including its
// zero-as-unset convention. UTC also removes Go's monotonic-clock component.
func normalizeRecordTimes(body *MemoryRecordBody) {
	body.CreatedAt = fromMs(toMs(body.CreatedAt)).UTC()
	body.UpdatedAt = fromMs(toMs(body.UpdatedAt)).UTC()
	body.ExpiresAt = fromMs(toMs(body.ExpiresAt)).UTC()
	body.DeletedAt = fromMs(toMs(body.DeletedAt)).UTC()
}

// validateRecordContent applies only to created/replacement content. Historical
// content remains usable under the aggregate bound when it is not replaced.
func validateRecordContent(content string) error {
	if len(content) > 4096 {
		return ErrContentTooLong
	}
	return nil
}

// canonicalRecordBody never repairs metadata. Callers normalize metadata only
// at creation, explicit replacement, or authorized legacy import.
func canonicalRecordBody(body MemoryRecordBody) ([]byte, error) {
	total := len(body.Metadata)
	for _, value := range []string{
		body.ID, string(body.Kind), body.Content, body.Namespace, body.WorkspaceID, body.SessionID,
		body.Provenance.SourceKind, body.Provenance.SourceID, body.Provenance.Hash,
		body.Provenance.OriginTool, body.Provenance.OriginSessionID, string(body.Provenance.TrustClass),
	} {
		// Subtract before adding so even pathological inputs cannot overflow total.
		if total > 32*1024 || len(value) > 32*1024-total {
			return nil, ErrRecordTooLarge
		}
		total += len(value)
	}
	switch body.Provenance.TrustClass {
	case TrustAgentWritten, TrustLegacyUnreviewed:
	default:
		return nil, errInvalidRecordTrust
	}
	// MarshalCanonical would otherwise encode a nil RawMessage as JSON null,
	// repairing an empty stored value into a valid, different one.
	if len(body.Metadata) == 0 {
		return nil, ErrBadMetadata
	}
	canonical, err := signing.MarshalCanonical(body)
	if err != nil {
		// Parser errors may contain stored data. Retain only bounded categories.
		for _, reason := range []error{signing.ErrInvalidUTF8, signing.ErrInvalidUnicodeEscape, signing.ErrDuplicateKey, signing.ErrTrailingData} {
			if errors.Is(err, reason) {
				return nil, reason
			}
		}
		return nil, errInvalidRecordBody
	}
	return canonical, nil
}

func signRecord(ctx context.Context, signer signing.Signer, record *MemoryRecord) error {
	if ctx == nil {
		return errors.New("memory: nil signing context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if signer == nil {
		return signing.ErrUninitializedKey
	}
	body := record.MemoryRecordBody
	normalizeRecordTimes(&body)
	canonical, err := canonicalRecordBody(body)
	if err != nil {
		return err
	}
	signature, err := signer.Sign(ctx, MemoryRecordDomain, canonical)
	if err != nil {
		return err
	}
	record.MemoryRecordBody, record.Signature = body, signature
	return nil
}

func verifyRecord(ctx context.Context, ring *signing.Keyring, record MemoryRecord) error {
	if ctx == nil {
		return errors.New("memory: nil verification context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	normalizeRecordTimes(&record.MemoryRecordBody)
	canonical, err := canonicalRecordBody(record.MemoryRecordBody)
	if err == nil {
		if len(record.Signature.Bytes) == 0 {
			err = errMissingRecordSignature
		} else {
			err = ring.Verify(ctx, MemoryRecordDomain, canonical, record.Signature)
		}
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrRecordIntegrity, err)
}
