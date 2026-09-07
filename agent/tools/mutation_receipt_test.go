package tools

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/signing"
)

// Public RFC 8032 section 7.1 test key; never use it outside tests. The body,
// complete frame, and signature were calculated independently with stdlib only.
const receiptSeed = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
const receiptPublicKey = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
const receiptKeyID = "b0f0e4099cd739f05a2defb07e1940a08ffabcfc8ce64a4e6deeaa9365b4bf00"
const receiptEmptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
const receiptBodyJSON = `{"after_hash":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad","after_mode":420,"agent_id":"b0f0e4099cd739f05a2defb07e1940a08ffabcfc8ce64a4e6deeaa9365b4bf00","before_hash":"absent","kind":"intent","mutation_id":"AAAAAAAAAAAAAAAAAAAAAAAAAA","path":"src/main.go","timestamp":"2026-09-05T12:34:56.123456789Z","undo_of":"","workspace_hash":"c52ddf65534b7b46035084358ab7902be4bfef220bdb503ac7039cc861905b05"}`
const receiptFrame = "go-llm-signing-v1\x00\x00\x00\x00\x00\x00\x00\x00\x1ago-llm/mutation-receipt/v1{\"after_hash\":\"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad\",\"after_mode\":420,\"agent_id\":\"b0f0e4099cd739f05a2defb07e1940a08ffabcfc8ce64a4e6deeaa9365b4bf00\",\"before_hash\":\"absent\",\"kind\":\"intent\",\"mutation_id\":\"AAAAAAAAAAAAAAAAAAAAAAAAAA\",\"path\":\"src/main.go\",\"timestamp\":\"2026-09-05T12:34:56.123456789Z\",\"undo_of\":\"\",\"workspace_hash\":\"c52ddf65534b7b46035084358ab7902be4bfef220bdb503ac7039cc861905b05\"}"
const receiptSignature = "8nl//cjzEis32LmLH+TK+Vej5oIf+So670gRzhG/13K2/QtUcXxka/Zysbg/m/I5iACtIU7HWHxrHlysOYgkAg=="

func receiptFixture(t *testing.T) (MutationReceipt, *signing.Ed25519Signer) {
	t.Helper()
	seed, err := hex.DecodeString(receiptSeed)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signing.NewEd25519Signer(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	var body MutationReceiptBody
	if err := json.Unmarshal([]byte(receiptBodyJSON), &body); err != nil {
		t.Fatal(err)
	}
	sig, err := base64.StdEncoding.DecodeString(receiptSignature)
	if err != nil {
		t.Fatal(err)
	}
	return MutationReceipt{Body: body, Signature: signing.Signature{Alg: signing.AlgEd25519, KeyID: receiptKeyID, Bytes: sig}}, signer
}

func receiptJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMutationReceiptGolden(t *testing.T) {
	t.Parallel()
	receipt, signer := receiptFixture(t)
	pub, err := hex.DecodeString(receiptPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(receiptFrame), receipt.Signature.Bytes) {
		t.Fatal("literal fixture signature does not authenticate literal frame")
	}
	got, err := signing.MarshalCanonical(receipt.Body)
	if err != nil || string(got) != receiptBodyJSON {
		t.Fatalf("canonical body = %s, %v; want literal body", got, err)
	}
	signed, err := SignMutationReceipt(context.Background(), signer, receipt.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.StdEncoding.EncodeToString(signed.Signature.Bytes); got != receiptSignature {
		t.Fatalf("SignMutationReceipt signature = %s, want %s", got, receiptSignature)
	}
	if signed.Signature.KeyID != receiptKeyID || signed.Signature.Alg != signing.AlgEd25519 {
		t.Fatalf("SignMutationReceipt binding = %+v, want RFC fixture identity", signed.Signature)
	}
	if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), receipt); err != nil {
		t.Fatalf("VerifyMutationReceipt(literal fixture) = %v", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, receiptJSON(t, receipt), "", "  "); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMutationReceipt(append(pretty.Bytes(), '\n'))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), decoded); err != nil {
		t.Fatalf("VerifyMutationReceipt(whitespace/member-order variant) = %v", err)
	}
}

func TestMutationReceiptSignedFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*MutationReceiptBody)
	}{
		{"kind", func(b *MutationReceiptBody) { b.Kind = "applied" }},
		{"mutation_id", func(b *MutationReceiptBody) { b.MutationID = strings.Repeat("B", 26) }},
		{"workspace_hash", func(b *MutationReceiptBody) { b.WorkspaceHash = receiptEmptyHash }},
		{"path", func(b *MutationReceiptBody) { b.Path = "src/other.go" }},
		{"before_hash", func(b *MutationReceiptBody) { b.BeforeHash = receiptEmptyHash }},
		{"after_hash", func(b *MutationReceiptBody) { b.AfterHash = receiptEmptyHash }},
		{"timestamp", func(b *MutationReceiptBody) { b.Timestamp = "2026-09-04T00:00:00Z" }},
		{"agent_id", func(b *MutationReceiptBody) { b.AgentID = receiptEmptyHash }},
		{"after_mode", func(b *MutationReceiptBody) { mode := uint32(0); b.AfterMode = &mode }},
		{"undo_of", func(b *MutationReceiptBody) { b.UndoOf = strings.Repeat("B", 26) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt, signer := receiptFixture(t)
			// Null mode makes the before_hash/undo_of changes semantically valid,
			// so a signature failure cannot be masked by mode validation.
			receipt.Body.AfterMode = nil
			var err error
			receipt, err = SignMutationReceipt(context.Background(), signer, receipt.Body)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&receipt.Body)
			if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), receipt); err == nil {
				t.Fatalf("VerifyMutationReceipt(tampered %s) = nil, want rejection", tc.name)
			}
		})
	}
	// Intent/applied substitution must fail in both directions.
	receipt, signer := receiptFixture(t)
	receipt.Body.Kind = "applied"
	receipt, err := SignMutationReceipt(context.Background(), signer, receipt.Body)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Body.Kind = "intent"
	if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), receipt); err == nil {
		t.Fatal("VerifyMutationReceipt(applied substituted with intent) = nil")
	}
}

func TestMutationReceiptSignFailures(t *testing.T) {
	receipt, signer := receiptFixture(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name   string
		ctx    context.Context
		signer signing.Signer
		want   error
	}{
		{"nil context", nil, signer, nil},
		{"canceled", canceled, signer, context.Canceled},
		{"nil signer", context.Background(), nil, nil},
		{"typed nil signer", context.Background(), (*signing.Ed25519Signer)(nil), nil},
		{"uninitialized signer", context.Background(), &signing.Ed25519Signer{}, nil},
		{"signer failure", context.Background(), receiptFailSigner{signer}, errReceiptSigner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SignMutationReceipt(tc.ctx, tc.signer, receipt.Body)
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("SignMutationReceipt(%s) = %v, want rejection wrapping %v", tc.name, err, tc.want)
			}
		})
	}
}

var errReceiptSigner = errors.New("receipt signer unavailable")

type receiptFailSigner struct{ signing.Signer }

func (receiptFailSigner) Sign(context.Context, string, []byte) (signing.Signature, error) {
	return signing.Signature{}, errReceiptSigner
}

type receiptResultSigner struct {
	signing.Signer
	result signing.Signature
}

func (s receiptResultSigner) Sign(context.Context, string, []byte) (signing.Signature, error) {
	return s.result, nil
}

func TestMutationReceiptRejectsMalformedSignerResult(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"key", "algorithm", "length"} {
		t.Run(change, func(t *testing.T) {
			receipt, signer := receiptFixture(t)
			switch change {
			case "key":
				receipt.Signature.KeyID = receiptEmptyHash
			case "algorithm":
				receipt.Signature.Alg = signing.AlgHMACSHA256
				receipt.Signature.Bytes = receipt.Signature.Bytes[:32]
			case "length":
				receipt.Signature.Bytes = nil
			}
			if _, err := SignMutationReceipt(context.Background(), receiptResultSigner{signer, receipt.Signature}, receipt.Body); err == nil {
				t.Fatalf("SignMutationReceipt(malformed signer %s) = nil, want rejection", change)
			}
		})
	}
}

func TestMutationReceiptOwnsSignedMode(t *testing.T) {
	t.Parallel()
	receipt, signer := receiptFixture(t)
	signed, err := SignMutationReceipt(context.Background(), signer, receipt.Body)
	if err != nil {
		t.Fatal(err)
	}
	*receipt.Body.AfterMode = 0
	if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), signed); err != nil {
		t.Fatalf("VerifyMutationReceipt(after caller changes input mode) = %v, want original signed body", err)
	}
}

func TestMutationReceiptRequiredMembers(t *testing.T) {
	t.Parallel()
	receipt, _ := receiptFixture(t)
	for _, scope := range []string{"envelope", "body", "signature"} {
		var fields map[string]json.RawMessage
		raw := receiptJSON(t, receipt)
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if scope != "envelope" {
			raw = fields[scope]
			fields = nil
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
		}
		for member := range fields {
			for _, change := range []string{"omitted", "null", "case variant", "unknown", "wrong type", "duplicate"} {
				if member == "after_mode" && change == "null" {
					continue
				}
				t.Run(scope+"/"+member+"/"+change, func(t *testing.T) {
					var edited map[string]json.RawMessage
					if err := json.Unmarshal(raw, &edited); err != nil {
						t.Fatal(err)
					}
					switch change {
					case "omitted":
						delete(edited, member)
					case "null":
						edited[member] = json.RawMessage(`null`)
					case "case variant":
						edited[strings.ToUpper(member)] = edited[member]
						delete(edited, member)
					case "unknown":
						edited[member+"_extra"] = json.RawMessage(`"unsigned"`)
					case "wrong type":
						edited[member] = json.RawMessage(`true`)
					}
					modified := receiptJSON(t, edited)
					if change == "duplicate" {
						modified = append([]byte(`{"`+member+`":`+string(fields[member])+`,`), modified[1:]...)
					}
					if scope != "envelope" {
						var envelope map[string]json.RawMessage
						if err := json.Unmarshal(receiptJSON(t, receipt), &envelope); err != nil {
							t.Fatal(err)
						}
						envelope[scope] = modified
						modified = receiptJSON(t, envelope)
					}
					if _, err := DecodeMutationReceipt(modified); err == nil {
						t.Fatalf("DecodeMutationReceipt(%s %s %s) = nil, want rejection", scope, member, change)
					}
				})
			}
		}
	}
}

func TestMutationReceiptInvalidBodies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*MutationReceiptBody)
	}{
		{"kind empty", func(b *MutationReceiptBody) { b.Kind = "" }},
		{"kind unknown", func(b *MutationReceiptBody) { b.Kind = "complete" }},
		{"kind case", func(b *MutationReceiptBody) { b.Kind = "Intent" }},
		{"id empty", func(b *MutationReceiptBody) { b.MutationID = "" }},
		{"id short", func(b *MutationReceiptBody) { b.MutationID = strings.Repeat("A", 25) }},
		{"id lowercase", func(b *MutationReceiptBody) { b.MutationID = strings.Repeat("a", 26) }},
		{"id punctuation", func(b *MutationReceiptBody) { b.MutationID = strings.Repeat("A", 26) + "/" }},
		{"id digit zero", func(b *MutationReceiptBody) { b.MutationID = strings.Repeat("A", 26) + "0" }},
		{"undo id short", func(b *MutationReceiptBody) { b.AfterMode = nil; b.UndoOf = "A" }},
		{"undo id invalid", func(b *MutationReceiptBody) { b.AfterMode = nil; b.UndoOf = strings.Repeat("A", 26) + "\n" }},
		{"undo self lineage", func(b *MutationReceiptBody) { b.AfterMode = nil; b.UndoOf = b.MutationID }},
		{"workspace absent", func(b *MutationReceiptBody) { b.WorkspaceHash = "absent" }},
		{"workspace empty", func(b *MutationReceiptBody) { b.WorkspaceHash = "" }},
		{"workspace uppercase", func(b *MutationReceiptBody) { b.WorkspaceHash = strings.ToUpper(b.WorkspaceHash) }},
		{"workspace short", func(b *MutationReceiptBody) { b.WorkspaceHash = b.WorkspaceHash[:63] }},
		{"before empty", func(b *MutationReceiptBody) { b.AfterMode = nil; b.BeforeHash = "" }},
		{"before invalid", func(b *MutationReceiptBody) { b.AfterMode = nil; b.BeforeHash = strings.Repeat("g", 64) }},
		{"after empty", func(b *MutationReceiptBody) { b.AfterHash = "" }},
		{"after uppercase", func(b *MutationReceiptBody) { b.AfterHash = strings.ToUpper(b.AfterHash) }},
		{"both absent", func(b *MutationReceiptBody) { b.AfterMode = nil; b.AfterHash = "absent" }},
		{"path empty", func(b *MutationReceiptBody) { b.Path = "" }},
		{"path root", func(b *MutationReceiptBody) { b.Path = "." }},
		{"path absolute", func(b *MutationReceiptBody) { b.Path = "/src/main.go" }},
		{"path drive", func(b *MutationReceiptBody) { b.Path = "C:/src/main.go" }},
		{"path backslash", func(b *MutationReceiptBody) { b.Path = `src\main.go` }},
		{"path dot", func(b *MutationReceiptBody) { b.Path = "./src/main.go" }},
		{"path inner dot", func(b *MutationReceiptBody) { b.Path = "src/./main.go" }},
		{"path parent", func(b *MutationReceiptBody) { b.Path = "../main.go" }},
		{"path cleaned parent", func(b *MutationReceiptBody) { b.Path = "src/../main.go" }},
		{"path duplicate slash", func(b *MutationReceiptBody) { b.Path = "src//main.go" }},
		{"path trailing slash", func(b *MutationReceiptBody) { b.Path = "src/" }},
		{"path nul", func(b *MutationReceiptBody) { b.Path = "src/\x00main.go" }},
		{"time empty", func(b *MutationReceiptBody) { b.Timestamp = "" }},
		{"time malformed", func(b *MutationReceiptBody) { b.Timestamp = "yesterday" }},
		{"time offset", func(b *MutationReceiptBody) { b.Timestamp = "2026-09-05T12:34:56+00:00" }},
		{"time non UTC", func(b *MutationReceiptBody) { b.Timestamp = "2026-09-05T12:34:56-04:00" }},
		{"time trailing zero", func(b *MutationReceiptBody) { b.Timestamp = "2026-09-05T12:34:56.100Z" }},
		{"time comma", func(b *MutationReceiptBody) { b.Timestamp = "2026-09-05T12:34:56,1Z" }},
		{"time excess precision", func(b *MutationReceiptBody) { b.Timestamp = "2026-09-05T12:34:56.1234567891Z" }},
		{"agent empty", func(b *MutationReceiptBody) { b.AgentID = "" }},
		{"agent mismatch", func(b *MutationReceiptBody) { b.AgentID = receiptEmptyHash }},
		{"mode non permission", func(b *MutationReceiptBody) { mode := uint32(0o1000); b.AfterMode = &mode }},
		{"mode overwrite", func(b *MutationReceiptBody) { b.BeforeHash = receiptEmptyHash }},
		{"mode inverse", func(b *MutationReceiptBody) { b.UndoOf = strings.Repeat("B", 26) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt, signer := receiptFixture(t)
			tc.edit(&receipt.Body)
			if _, err := SignMutationReceipt(context.Background(), signer, receipt.Body); err == nil {
				t.Fatalf("SignMutationReceipt(%s) = nil, want rejection", tc.name)
			}
			// Bypass the receipt constructor: a valid signature over invalid
			// semantics must still be rejected by both consumer entry points.
			payload, err := signing.MarshalCanonical(receipt.Body)
			if err != nil {
				t.Fatal(err)
			}
			receipt.Signature, err = signer.Sign(context.Background(), "go-llm/mutation-receipt/v1", payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeMutationReceipt(receiptJSON(t, receipt)); err == nil {
				t.Fatalf("DecodeMutationReceipt(%s) = nil, want rejection", tc.name)
			}
			if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), receipt); err == nil {
				t.Fatalf("VerifyMutationReceipt(authentic %s) = nil, want rejection", tc.name)
			}
		})
	}
}

func TestMutationReceiptWireValidation(t *testing.T) {
	t.Parallel()
	receipt, _ := receiptFixture(t)
	raw := string(receiptJSON(t, receipt))
	cases := map[string]string{
		"empty": "", "not object": `[]`, "null": `null`, "trailing": raw + ` {}`,
		"truncated":          raw[:len(raw)-1],
		"invalid utf8":       strings.Replace(raw, "src/main.go", "src/\xff", 1),
		"unpaired surrogate": strings.Replace(raw, "src/main.go", `src/\ud800`, 1),
		"escaped duplicate":  strings.Replace(raw, `"kind":"intent"`, `"kind":"intent","\u006bind":"intent"`, 1),
	}
	for _, mode := range []string{`-1`, `512`, `4294967296`, `1.5`, `420.0`, `4.2e2`, `-0`, `"420"`, `true`, `[]`, `{}`} {
		cases["mode "+mode] = strings.Replace(raw, `"after_mode":420`, `"after_mode":`+mode, 1)
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMutationReceipt([]byte(input)); err == nil {
				t.Fatalf("DecodeMutationReceipt(%s) = nil, want rejection", name)
			}
		})
	}
}

func TestMutationReceiptModesAndAbsence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                      string
		before, after, undo, mode string
	}{
		{"tracked zero", "absent", receiptEmptyHash, "", `0`},
		{"tracked maximum", "absent", receiptEmptyHash, "", `511`},
		{"untracked create", "absent", receiptEmptyHash, "", `null`},
		{"empty file overwrite", receiptEmptyHash, receiptEmptyHash, "", `null`},
		{"undo deletion", receiptEmptyHash, "absent", strings.Repeat("B", 26), `null`},
		{"inverse create", "absent", receiptEmptyHash, strings.Repeat("B", 26), `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt, signer := receiptFixture(t)
			receipt.Body.BeforeHash, receipt.Body.AfterHash, receipt.Body.UndoOf = tc.before, tc.after, tc.undo
			if err := json.Unmarshal([]byte(tc.mode), &receipt.Body.AfterMode); err != nil {
				t.Fatal(err)
			}
			receipt.Body.Timestamp = "2026-09-04T00:00:00Z"              // no assumed clock monotonicity
			receipt.Body.MutationID = strings.Repeat("A", 26) + "234567" // future rand.Text lengths
			signed, err := SignMutationReceipt(context.Background(), signer, receipt.Body)
			if err != nil {
				t.Fatal(err)
			}
			raw := receiptJSON(t, signed)
			if !bytes.Contains(raw, []byte(`"after_mode":`+tc.mode)) {
				t.Fatalf("signed mode = %s, want %s", raw, tc.mode)
			}
			decoded, err := DecodeMutationReceipt(raw)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Body.BeforeHash != tc.before || decoded.Body.AfterHash != tc.after {
				t.Fatalf("decoded hashes = %s/%s, want %s/%s", decoded.Body.BeforeHash, decoded.Body.AfterHash, tc.before, tc.after)
			}
			if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), decoded); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMutationReceiptSizeBound(t *testing.T) {
	t.Parallel()
	receipt, _ := receiptFixture(t)
	raw := receiptJSON(t, receipt)
	padded := append(raw, bytes.Repeat([]byte{' '}, 32*1024-len(raw))...)
	if _, err := DecodeMutationReceipt(padded); err != nil {
		t.Fatalf("DecodeMutationReceipt(32768 bytes) = %v, want accepted", err)
	}
	if _, err := DecodeMutationReceipt(append(padded, ' ')); err == nil {
		t.Fatal("DecodeMutationReceipt(32769 bytes) = nil, want size rejection")
	}
	if _, err := DecodeMutationReceipt(bytes.Repeat([]byte{'!'}, 32*1024+1)); err == nil || !strings.Contains(err.Error(), "32768") {
		t.Fatalf("DecodeMutationReceipt(oversized invalid JSON) = %v, want size rejection before JSON parsing", err)
	}
}

func TestMutationReceiptTypedSizeBound(t *testing.T) {
	t.Parallel()
	receipt, signer := receiptFixture(t)
	// Struct field order does not affect length; ASCII avoids escape changes.
	receipt.Body.Path += strings.Repeat("x", 32*1024-len(receiptJSON(t, receipt)))
	signed, err := SignMutationReceipt(context.Background(), signer, receipt.Body)
	if err != nil {
		t.Fatalf("SignMutationReceipt(32768-byte envelope) = %v", err)
	}
	raw := receiptJSON(t, signed)
	if len(raw) != 32*1024 {
		t.Fatalf("signed envelope length = %d, want 32768", len(raw))
	}
	decoded, err := DecodeMutationReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), decoded); err != nil {
		t.Fatalf("VerifyMutationReceipt(32768-byte envelope) = %v", err)
	}
	receipt.Body.Path += "x"
	if _, err := SignMutationReceipt(context.Background(), signer, receipt.Body); err == nil {
		t.Error("SignMutationReceipt(32769-byte envelope) = nil, want size rejection")
	}
	payload, err := signing.MarshalCanonical(receipt.Body)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Signature, err = signer.Sign(context.Background(), MutationReceiptDomain, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), receipt); err == nil {
		t.Error("VerifyMutationReceipt(authentic 32769-byte envelope) = nil, want size rejection")
	}
}

func TestMutationReceiptSignatureValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*signing.Signature)
	}{
		{"unknown algorithm", func(s *signing.Signature) { s.Alg = "none" }},
		{"empty key", func(s *signing.Signature) { s.KeyID = "" }},
		{"wrong key", func(s *signing.Signature) { s.KeyID = receiptEmptyHash }},
		{"uppercase key", func(s *signing.Signature) { s.KeyID = strings.ToUpper(s.KeyID) }},
		{"empty signature", func(s *signing.Signature) { s.Bytes = []byte{} }},
		{"short signature", func(s *signing.Signature) { s.Bytes = s.Bytes[:63] }},
		{"long signature", func(s *signing.Signature) { s.Bytes = append(s.Bytes, 0) }},
		{"wrong algorithm", func(s *signing.Signature) { s.Alg = signing.AlgHMACSHA256 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt, signer := receiptFixture(t)
			tc.edit(&receipt.Signature)
			if _, err := DecodeMutationReceipt(receiptJSON(t, receipt)); err == nil {
				t.Fatalf("DecodeMutationReceipt(%s) = nil, want rejection", tc.name)
			}
			if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), receipt); err == nil {
				t.Fatalf("VerifyMutationReceipt(%s) = nil, want rejection", tc.name)
			}
		})
	}
	receipt, signer := receiptFixture(t)
	for _, encoded := range []string{`?`, receiptSignature + `\n`, receiptSignature[:len(receiptSignature)-3] + `h==`} {
		raw := strings.Replace(string(receiptJSON(t, receipt)), receiptSignature, encoded, 1)
		if _, err := DecodeMutationReceipt([]byte(raw)); err == nil {
			t.Fatalf("DecodeMutationReceipt(noncanonical base64 %q) = nil", encoded)
		}
	}
	for _, domain := range []string{"go-llm/mutation-receipt/v2", "go-llm/other/v1"} {
		sig, err := signer.Sign(context.Background(), domain, []byte(receiptBodyJSON))
		if err != nil {
			t.Fatal(err)
		}
		receipt.Signature = sig
		if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), receipt); err == nil {
			t.Fatalf("VerifyMutationReceipt(signature for %s) = nil", domain)
		}
	}
	receipt, _ = receiptFixture(t)
	receipt.Signature.Bytes[0] ^= 1
	if err := VerifyMutationReceipt(context.Background(), signer.Verifier(), receipt); err == nil {
		t.Fatal("VerifyMutationReceipt(corrupt signature) = nil")
	}
}

func TestMutationReceiptHMACAndVerifierFailures(t *testing.T) {
	t.Parallel()
	receipt, edSigner := receiptFixture(t)
	hmacSigner, err := signing.NewHMAC(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	receipt.Body.AgentID = hmacSigner.KeyID()
	receipt, err = SignMutationReceipt(context.Background(), hmacSigner, receipt.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMutationReceipt(receiptJSON(t, receipt))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMutationReceipt(context.Background(), hmacSigner, decoded); err != nil {
		t.Fatalf("VerifyMutationReceipt(HMAC) = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		verifier signing.Verifier
	}{
		{"nil context", nil, hmacSigner},
		{"canceled", canceled, hmacSigner},
		{"nil verifier", context.Background(), nil},
		{"typed nil verifier", context.Background(), (*signing.Ed25519Verifier)(nil)},
		{"wrong key and algorithm", context.Background(), edSigner.Verifier()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyMutationReceipt(tc.ctx, tc.verifier, decoded); err == nil {
				t.Fatalf("VerifyMutationReceipt(%s) = nil, want rejection", tc.name)
			}
		})
	}
}
