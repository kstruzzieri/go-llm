// Package signing provides detached signatures over canonical bytes for the
// zero-trust ledgers (epic #429, ZT-301): signed mutation receipts, memory
// provenance, the golem audit verifier, and verified delegate proposals.
// The package knows nothing about those record schemas. Consumers own the
// schema; this package owns canonicalization, domain separation, key
// identity, rotation, and key storage.
//
// # Usage
//
//	signer, created, err := signing.LoadOrCreateEd25519(keyPath)
//	if err != nil { return err }
//	if created { reportNewIdentity(signer.KeyID()) }
//	body, err := signing.MarshalCanonical(record.Body)
//	if err != nil { return err }
//	sig, err := signer.Sign(ctx, "go-llm/mutation-receipt/v1", body)
//	if err != nil { return err }
//	record.Sig = sig // JSON: {"alg":"ed25519","kid":"<hex>","sig":"<base64>"}
//
//	// verify (any process holding the public keys)
//	ring, err := signing.NewKeyring(verifierA, verifierB)
//	if err != nil { return err }
//	body, err = signing.MarshalCanonical(record.Body)
//	if err != nil { return err }
//	if err := ring.Verify(ctx, "go-llm/mutation-receipt/v1", body, record.Sig); err != nil {
//	    // fail closed on ANY error
//	}
//
// # What to sign
//
// Put every signed field inside one body value and store Signature as its
// sibling. Canonicalize the whole body; do not maintain a mirror struct or
// merely zero a signature field that is still serialized. This makes added
// body fields signed by default and lets audit code canonicalize a raw body
// without knowing its schema. Normalize time.Time to UTC before marshaling:
// encoding/json preserves the zone offset, so the same instant can marshal
// differently. json.RawMessage fields are normalized. Keep one domain
// constant beside each body schema and bump its version when signed meaning
// changes.
//
// # Canonical form v1
//
// Canonicalize and MarshalCanonical produce: object keys sorted by Go string
// order (UTF-8 byte order) at every depth; no insignificant whitespace;
// number literals verbatim (9007199254740993 survives exactly; "1.0" and
// "1" are distinct literals, though json.Marshal never emits "1.0" for a Go
// float); minimal string escaping with HTML escaping off, except U+2028 and
// U+2029 which encoding/json always escapes. Rejected: invalid UTF-8
// (ErrInvalidUTF8), unpaired UTF-16 surrogate escapes
// (ErrInvalidUnicodeEscape), duplicate object keys at any depth
// (ErrDuplicateKey), data after the value (ErrTrailingData), empty input. MarshalCanonical also
// rejects invalid reachable strings and string map keys before encoding/json
// can replace them with U+FFFD. This conservative walk includes fields JSON
// may omit. Unicode is not normalized: NFC and NFD remain distinct.
//
// This is NOT RFC 8785 (JCS). Divergences: key order is UTF-8 not UTF-16
// (differs only when keys mix U+E000..U+FFFF with supplementary-plane
// characters); U+2028/2029 are escaped; numbers are verbatim rather than
// ES6-normalized. Golden vectors in canonical_test.go pin the exact bytes;
// if a Go toolchain changes encoding/json output they go red. This API stays
// v1 forever; a future incompatible form gets a new function and consumer
// record/domain version.
//
// Custom json.Marshaler and encoding.TextMarshaler implementations own their
// determinism and pre-coercion UTF-8 behavior. Avoid them in signed bodies
// unless their output has separate collision and determinism tests.
//
// # Backends
//
// Ed25519 (pure, RFC 8032) for provenance across processes: publish the
// public key, verify anywhere. HMAC-SHA256 for same-process integrity:
// verification is symmetric, so anyone who can verify can also sign. Both
// sign frame(domain, payload), never bare bytes, so a signature over one
// record kind never verifies as another. Verifiers reject a signature naming
// a different key id or algorithm (ErrKeyMismatch) before any crypto, which
// blocks algorithm confusion. HMAC tags are compared with hmac.Equal only;
// constant_time_test.go pins the call site.
//
// Signer and Verifier are sibling capabilities: an Ed25519 signer exposes its
// public-only Verifier explicitly, while HMAC necessarily implements both.
// Local backends honor an already-cancelled ctx before crypto; SSH-agent and
// KMS backends land as separate tickets. Exported concrete zero values return
// ErrUninitializedKey from cryptographic operations rather than panic.
// Signer and Verifier values are immutable after construction and safe for
// concurrent use; Keyring is not (populate it before sharing).
//
// Cryptographic validity is not authorization. A Keyring proves that one of
// its members signed the bytes; callers use purpose-scoped rings to decide
// which keys and algorithms are allowed for a domain. Never populate a trust
// ring from keys embedded in the untrusted record being verified.
//
// # Keys
//
// KeyID is one versioned algorithm-bound SHA-256 derivation over public key
// material (Ed25519) or the secret (HMAC), never assigned. HMAC keys must be
// generated high-entropy bytes, never padded passwords. Rotation is one
// current Signer plus a purpose-scoped Keyring of retained trusted verifiers;
// consumers persist that trust mapping. Keyring is populated before use and
// has no mutex.
//
// LoadOrCreateEd25519 / LoadOrCreateHMAC return created=true only to the
// process that publishes a new identity. They validate and anchor an
// owner-only key directory with os.Root, make its parent entry durable before
// generation, publish a synced 0600 sibling temp through an atomic no-replace
// hard link, fsync the directory, and strictly decode typed PEM. Loads of an
// existing key fsync the directory again before trusting it, so a publish
// whose final sync failed keeps reporting ErrKeyFileDurability on retry until
// the entry is known to be durable. Loads refuse symlinks, swaps, loose unix
// ownership/modes, foreign key types, and oversized files. The key directory must sit below a caller-trusted ancestor; only its
// immediate directory is validated here. Windows relies on profile ACLs.
// Signer values implement fmt.Formatter and never print key material.
//
// Per-record signatures do not detect replay, deletion, reordering, rollback,
// or key loss. Ledgers put sequence/previous-hash fields inside the signed
// body or use another trusted checkpoint. Zeroization is not promised; Go
// cannot guarantee it. Consumers cap untrusted record sizes before calling
// this package; frame construction makes one payload-sized allocation.
package signing
