package signing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
)

// ed25519Key is the key material both Ed25519 types point at. fmt.Formatter
// controls normal formatting; pointer indirection is defense in depth for
// reflection-based formatting that ignores it. Pinned by TestEd25519Redaction.
type ed25519Key struct {
	priv ed25519.PrivateKey // nil for verify-only
	pub  ed25519.PublicKey
	kid  string
}

// Ed25519Verifier verifies signatures made by one Ed25519 key.
type Ed25519Verifier struct{ k *ed25519Key }

var _ Verifier = (*Ed25519Verifier)(nil)

// NewEd25519Verifier wraps a 32-byte public key. The length is validated here
// because ed25519.Verify panics on a wrong-length key. The key is cloned.
func NewEd25519Verifier(pub ed25519.PublicKey) (*Ed25519Verifier, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("signing: ed25519 public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	pub = bytes.Clone(pub)
	return &Ed25519Verifier{k: &ed25519Key{pub: pub, kid: deriveKeyID(AlgEd25519, pub)}}, nil
}

// KeyID is the algorithm-bound digest of the public key.
func (v *Ed25519Verifier) KeyID() string {
	if v == nil || v.k == nil {
		return ""
	}
	return v.k.kid
}

// Algorithm is AlgEd25519.
func (v *Ed25519Verifier) Algorithm() Algorithm { return AlgEd25519 }

// PublicKey returns a copy of the public key.
func (v *Ed25519Verifier) PublicKey() ed25519.PublicKey {
	if v == nil || v.k == nil {
		return nil
	}
	return bytes.Clone(v.k.pub)
}

// Verify implements Verifier.
func (v *Ed25519Verifier) Verify(ctx context.Context, domain string, payload []byte, sig Signature) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if v == nil || v.k == nil {
		return ErrUninitializedKey
	}
	return ed25519Verify(v, v.k, domain, payload, sig)
}

// Format names the key without exposing it under any fmt verb.
func (v *Ed25519Verifier) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "signing.Ed25519Verifier(kid=%s)", v.KeyID())
}

func ed25519Verify(v Verifier, k *ed25519Key, domain string, payload []byte, sig Signature) error {
	if err := checkBinding(v, sig); err != nil {
		return err
	}
	msg, err := frame(domain, payload)
	if err != nil {
		return err
	}
	// ed25519.Verify returns false (no panic) for a wrong-length signature;
	// TestEd25519VerifyRejectsWrongLengthSignature pins that, so no guard.
	if !ed25519.Verify(k.pub, msg, sig.Bytes) {
		return ErrInvalidSignature
	}
	return nil
}

// Ed25519Signer signs with an Ed25519 private key. Verification is exposed as
// a separate least-authority value through Verifier.
type Ed25519Signer struct{ k *ed25519Key }

var _ Signer = (*Ed25519Signer)(nil)

// NewEd25519Signer wraps a 64-byte private key (seed || public key, the
// crypto/ed25519 layout). The key is cloned.
func NewEd25519Signer(priv ed25519.PrivateKey) (*Ed25519Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: ed25519 private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if !priv.Equal(ed25519.NewKeyFromSeed(priv.Seed())) {
		return nil, errors.New("signing: ed25519 private key public half does not match its seed")
	}
	priv = bytes.Clone(priv)
	pub := priv.Public().(ed25519.PublicKey)
	return &Ed25519Signer{k: &ed25519Key{priv: priv, pub: pub, kid: deriveKeyID(AlgEd25519, pub)}}, nil
}

// GenerateEd25519 makes a fresh key. A nil random means crypto/rand.
func GenerateEd25519(random io.Reader) (*Ed25519Signer, error) {
	_, priv, err := ed25519.GenerateKey(random)
	if err != nil {
		return nil, fmt.Errorf("signing: generate ed25519: %w", err)
	}
	return NewEd25519Signer(priv)
}

// KeyID is the algorithm-bound digest of the public key.
func (s *Ed25519Signer) KeyID() string {
	if s == nil || s.k == nil {
		return ""
	}
	return s.k.kid
}

// Algorithm is AlgEd25519.
func (s *Ed25519Signer) Algorithm() Algorithm { return AlgEd25519 }

// PublicKey returns a copy of the public key.
func (s *Ed25519Signer) PublicKey() ed25519.PublicKey {
	if s == nil || s.k == nil {
		return nil
	}
	return bytes.Clone(s.k.pub)
}

// Verifier returns the public half, for keyrings and for publishing.
func (s *Ed25519Signer) Verifier() *Ed25519Verifier {
	if s == nil || s.k == nil {
		return nil
	}
	return &Ed25519Verifier{k: &ed25519Key{pub: s.k.pub, kid: s.k.kid}}
}

// Sign implements Signer.
func (s *Ed25519Signer) Sign(ctx context.Context, domain string, payload []byte) (Signature, error) {
	if err := checkContext(ctx); err != nil {
		return Signature{}, err
	}
	if s == nil || s.k == nil {
		return Signature{}, ErrUninitializedKey
	}
	msg, err := frame(domain, payload)
	if err != nil {
		return Signature{}, err
	}
	return Signature{Alg: AlgEd25519, KeyID: s.k.kid, Bytes: ed25519.Sign(s.k.priv, msg)}, nil
}

// Format names the key without exposing it under any fmt verb.
func (s *Ed25519Signer) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "signing.Ed25519Signer(kid=%s)", s.KeyID())
}
