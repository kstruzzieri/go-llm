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
// without knowing its schema. Two encoding/json rules silently unsign data: an
// ambiguous embedded field (two embedded structs exporting the same name) is
// dropped entirely, and json:"-" or unexported fields are never serialized.
// Nothing is signed unless it appears in the marshaled JSON. Keep bodies
// small: canonicalization allocates roughly one hundred times the input
// transiently, so cap untrusted records at tens of kilobytes, not megabytes.
// Normalize time.Time to UTC before marshaling: encoding/json preserves the
// zone offset, so the same instant can marshal differently. json.RawMessage
// fields are normalized. Keep one domain constant beside each body schema and
// bump its version when signed meaning changes.
//
// # Canonical form v1
//
// Canonical form v1 is a canonical form over lexical JSON, not semantic JSON.
// Canonicalize accepts exactly one JSON value, with optional surrounding JSON
// whitespace, and emits compact UTF-8 JSON without a final newline. Decoded
// object names are rejected when duplicated and sorted by UTF-8 byte order at
// every depth; Unicode is not normalized, so NFC and NFD spellings are
// distinct names. Number lexemes are preserved verbatim, so 1, 1.0, and 1e0
// are distinct v1 identities; json.Number, json.RawMessage, raw input, and
// custom marshalers can each produce any spelling, and only the spelling that
// was signed verifies. Strings follow encoding/json escaping with HTML
// escaping disabled; U+2028 and U+2029 remain escaped. Rejected: invalid
// UTF-8 (ErrInvalidUTF8), unpaired UTF-16 surrogate escapes
// (ErrInvalidUnicodeEscape), malformed JSON, duplicate decoded names
// (ErrDuplicateKey), non-whitespace trailing data (ErrTrailingData), and
// empty input.
//
// MarshalCanonical additionally rejects invalid reachable Go strings and
// invalid encoding.TextMarshaler output before encoding/json can replace
// them with U+FFFD, mirroring encoding/json's marshaler dispatch: the static
// type of a field or element decides first (an interface type embedding
// json.Marshaler routes to MarshalJSON, one embedding only TextMarshaler
// routes to MarshalText), then the addressable receiver, then the value; map
// keys use MarshalText only. Intentional []byte base64 encoding is
// unaffected. Custom marshaler internals are not walked; determinism of
// custom output remains the implementer's responsibility.
//
// This is not RFC 8785 (JCS). JCS orders names by UTF-16 code units and uses
// the I-JSON/IEEE-754 number model, under which large or high-precision
// values are expected to travel as strings; v1 keeps the lexeme instead. JCS
// also leaves U+2028 and U+2029 unescaped. Positive and negative golden
// vectors in canonical_test.go pin the bytes; if a Go toolchain changes
// encoding/json output they go red. Canonicalize is frozen at v1: an
// incompatible change gets a new function and a new consumer record/domain
// version, never a change to the bytes this function emits.
//
// # Backends
//
// Ed25519 (pure, RFC 8032) for provenance across processes: publish the public
// key, verify anywhere. HMAC-SHA256 for same-process integrity: verification
// is symmetric, so anyone who can verify can also sign. Both sign
// frame(domain, payload), never bare bytes, so a signature over one record
// kind never verifies as another. Verifiers reject a signature naming a
// different key id or algorithm (ErrKeyMismatch) before any crypto, which
// blocks algorithm confusion. Signature decodes its JSON "sig" with strict
// base64, so a stored record has exactly one spelling of its signature. HMAC
// tags are compared with hmac.Equal only; constant_time_test.go pins the call
// site.
//
// Signer and Verifier are sibling capabilities: an API exposure boundary, not
// a claim that cryptographic authority is always separable. An Ed25519 signer
// exposes its public-only Verifier explicitly; HMAC verification uses the
// signing secret, so HMACSigner intentionally implements both. Local backends
// honor an already-cancelled ctx before crypto and return an error for a nil
// ctx. Context, KeyID, and Algorithm are the common minimum contract;
// SSH-agent and KMS adapters (tracked separately) must document their own
// cancellation, key-id mapping, algorithm, and failure semantics and pass the
// shared conformance suite. Exported concrete zero values return
// ErrUninitializedKey from cryptographic operations rather than panic. Signer
// and Verifier values are immutable after construction and safe for concurrent
// use; Keyring is not (populate it before sharing).
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
// hard link, fsync the directory, and strictly decode typed PEM (block type,
// one block, no headers, no surrounding data). A crash between the temp write
// and the hard link leaves an unpublished owner-only .<name>.tmp-* file that
// is never collected; the next run then publishes a new identity and reports
// created=true. Loads of an existing key fsync the directory again before
// trusting it, so a publish whose final sync failed keeps reporting
// ErrKeyFileDurability on retry until the entry is known to be durable. Loads
// refuse symlinks, swaps, loose unix ownership/modes, foreign key types, and
// oversized files. The key directory must sit below a caller-trusted ancestor;
// only its immediate directory is validated here. Windows relies on profile
// ACLs. Signer and verifier types implement fmt.Formatter on value and pointer
// receivers, so values held in structs, slices, maps, and interfaces print
// only their key id under every verb. Residual: a signer stored by value in an
// unexported field of another struct is formatted by reflection, which fmt
// cannot route through Format; hold signers by pointer there.
//
// Per-record signatures do not detect replay, deletion, reordering, rollback,
// or key loss. Ledgers put sequence/previous-hash fields inside the signed
// body or use another trusted checkpoint. Zeroization is not promised; Go
// cannot guarantee it. Consumers cap untrusted record sizes before calling
// this package; frame construction makes one payload-sized allocation.
package signing
