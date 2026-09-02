package signing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
)

// MinHMACKeySize is the smallest key NewHMAC accepts: the SHA-256 output size.
const MinHMACKeySize = 32

// hmacKey sits behind a pointer for the same fmt-redaction reason as
// ed25519Key. Pinned by TestHMACRedaction.
type hmacKey struct {
	key []byte
	kid string
}

// HMACSigner signs and verifies with one HMAC-SHA256 key. Verification is
// symmetric: anyone who can verify can also sign, so this backend suits
// same-process integrity (delegate proposals), not cross-party provenance.
type HMACSigner struct{ k *hmacKey }

var (
	_ Signer   = (*HMACSigner)(nil)
	_ Verifier = (*HMACSigner)(nil)
)

// NewHMAC wraps key, which must be at least MinHMACKeySize bytes. The key is
// cloned.
func NewHMAC(key []byte) (*HMACSigner, error) {
	if len(key) < MinHMACKeySize {
		return nil, fmt.Errorf("signing: hmac key is %d bytes, want at least %d", len(key), MinHMACKeySize)
	}
	key = bytes.Clone(key)
	return &HMACSigner{k: &hmacKey{key: key, kid: deriveKeyID(AlgHMACSHA256, key)}}, nil
}

// GenerateHMAC makes a fresh 32-byte key. A nil random means crypto/rand.
func GenerateHMAC(random io.Reader) (*HMACSigner, error) {
	if random == nil {
		random = rand.Reader
	}
	key := make([]byte, MinHMACKeySize)
	if _, err := io.ReadFull(random, key); err != nil {
		return nil, fmt.Errorf("signing: generate hmac key: %w", err)
	}
	return NewHMAC(key)
}

// KeyID is the domain-separated hash of the key.
func (m *HMACSigner) KeyID() string {
	if m == nil || m.k == nil {
		return ""
	}
	return m.k.kid
}

// Algorithm is AlgHMACSHA256.
func (m *HMACSigner) Algorithm() Algorithm { return AlgHMACSHA256 }

func (m *HMACSigner) tag(domain string, payload []byte) ([]byte, error) {
	if m == nil || m.k == nil {
		return nil, ErrUninitializedKey
	}
	msg, err := frame(domain, payload)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, m.k.key)
	_, _ = h.Write(msg)
	return h.Sum(nil), nil
}

// Sign implements Signer.
func (m *HMACSigner) Sign(ctx context.Context, domain string, payload []byte) (Signature, error) {
	if err := checkContext(ctx); err != nil {
		return Signature{}, err
	}
	t, err := m.tag(domain, payload)
	if err != nil {
		return Signature{}, err
	}
	return Signature{Alg: AlgHMACSHA256, KeyID: m.k.kid, Bytes: t}, nil
}

// Verify recomputes the tag and compares with hmac.Equal, the package's only
// permitted MAC comparison (constant_time_test.go pins this).
func (m *HMACSigner) Verify(ctx context.Context, domain string, payload []byte, sig Signature) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if m == nil || m.k == nil {
		return ErrUninitializedKey
	}
	if err := checkBinding(m, sig); err != nil {
		return err
	}
	want, err := m.tag(domain, payload)
	if err != nil {
		return err
	}
	if !hmac.Equal(want, sig.Bytes) {
		return ErrInvalidSignature
	}
	return nil
}

// Format names the key without exposing it under any fmt verb.
func (m *HMACSigner) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "signing.HMACSigner(kid=%s)", m.KeyID())
}
