package signing

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

// Algorithm names one signature algorithm.
type Algorithm string

const (
	// AlgEd25519 is pure Ed25519 (RFC 8032) over the signing frame.
	AlgEd25519 Algorithm = "ed25519"
	// AlgHMACSHA256 is HMAC-SHA256 over the signing frame.
	AlgHMACSHA256 Algorithm = "hmac-sha256"
)

// Signature is a detached signature. It embeds in JSON as
// {"alg":"ed25519","kid":"<hex>","sig":"<base64>"}. Consumers store it inside
// their own record and exclude that field when re-canonicalizing to verify.
type Signature struct {
	Alg   Algorithm `json:"alg"`
	KeyID string    `json:"kid"`
	Bytes []byte    `json:"sig"`
}

// Sentinel errors. Callers verifying records fail closed on ANY non-nil error
// from Verify; the distinctions below exist for diagnostics, not policy.
var (
	ErrInvalidSignature     = errors.New("signing: invalid signature")
	ErrKeyMismatch          = errors.New("signing: signature does not name this verifier's key and algorithm")
	ErrUnknownKey           = errors.New("signing: unknown key id")
	ErrUninitializedKey     = errors.New("signing: uninitialized signer or verifier")
	ErrEmptyDomain          = errors.New("signing: empty domain")
	ErrInvalidUTF8          = errors.New("signing: canonicalize: input is not valid UTF-8")
	ErrInvalidUnicodeEscape = errors.New("signing: canonicalize: unpaired UTF-16 surrogate escape")
	ErrDuplicateKey         = errors.New("signing: canonicalize: duplicate object key")
	ErrTrailingData         = errors.New("signing: canonicalize: data after the JSON value")
	ErrInsecureKeyFile      = errors.New("signing: key file permissions or ownership are insecure")
	ErrInsecureKeyDirectory = errors.New("signing: key directory is not an owner-only real directory")
	ErrKeyFileNotRegular    = errors.New("signing: key file is not a regular file")
	ErrKeyFileDurability    = errors.New("signing: key storage directory durability is uncertain")
)

// Verifier checks signatures made by one key.
type Verifier interface {
	// KeyID is the stable, key-derived identifier signatures carry as "kid".
	KeyID() string
	// Algorithm is the algorithm signatures carry as "alg".
	Algorithm() Algorithm
	// Verify returns nil only when sig was produced by this key over exactly
	// (domain, payload). Every other outcome is a non-nil error.
	Verify(ctx context.Context, domain string, payload []byte, sig Signature) error
}

// Signer produces signatures. It deliberately does not imply verification
// authority; asymmetric implementations expose a separate verifier.
type Signer interface {
	// KeyID is the stable, key-derived identifier signatures carry as "kid".
	KeyID() string
	// Algorithm is the algorithm signatures carry as "alg".
	Algorithm() Algorithm
	// Sign signs payload under domain. The domain separates record kinds so
	// a signature over one kind never verifies as another; it must be
	// non-empty. Convention: "go-llm/<record-kind>/v<n>".
	Sign(ctx context.Context, domain string, payload []byte) (Signature, error)
}

// frameHeader versions the frame layout, not the canonical form.
const frameHeader = "go-llm-signing-v1\x00"

// keyIDHeader versions the algorithm-bound key-ID derivation.
const keyIDHeader = "go-llm-signing-kid-v1\x00"

// frame is the byte string every backend signs:
//
//	frameHeader || uint64_be(len(domain)) || domain || payload
//
// The length prefix makes ("ab","c") and ("a","bc") distinct.
func frame(domain string, payload []byte) ([]byte, error) {
	if domain == "" {
		return nil, ErrEmptyDomain
	}
	b := make([]byte, 0, len(frameHeader)+8+len(domain)+len(payload))
	b = append(b, frameHeader...)
	b = binary.BigEndian.AppendUint64(b, uint64(len(domain)))
	b = append(b, domain...)
	b = append(b, payload...)
	return b, nil
}

// deriveKeyID returns the lowercase hex SHA-256 of a versioned,
// length-delimited algorithm name and its key material. Public-key material
// is used for asymmetric algorithms; HMAC necessarily uses its secret.
func deriveKeyID(alg Algorithm, material []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(keyIDHeader))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(alg)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(alg))
	_, _ = h.Write(material)
	return hex.EncodeToString(h.Sum(nil))
}

// checkContext gives every backend the same already-cancelled behavior. Local
// primitives are not interruptible once their small operation has started.
func checkContext(ctx context.Context) error { return ctx.Err() }

// checkBinding rejects a signature that names a different key or algorithm
// than v before any cryptographic work. Key IDs and algorithm names are
// public, so plain comparison is fine here.
func checkBinding(v Verifier, sig Signature) error {
	if sig.Alg != v.Algorithm() || sig.KeyID != v.KeyID() {
		return ErrKeyMismatch
	}
	return nil
}
