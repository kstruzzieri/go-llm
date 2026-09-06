package memory

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/signing"
)

const recordCanonicalGolden = `{"content":"remember <this>","created_at":"2026-09-05T10:20:30.123Z","deleted_at":"0001-01-01T00:00:00Z","expires_at":"2026-09-06T10:20:30.789Z","id":"record-1","kind":"working","metadata":{"a":{"a":null,"z":1.0},"z":1e0},"namespace":"notes","provenance":{"origin_session_id":"session-origin","origin_tool":"memory_create","source_end":9,"source_hash":"sha256:fixture","source_id":"source-1","source_kind":"conversation","source_start":2,"trust_class":"agent-written"},"session_id":"session-visible","updated_at":"2026-09-05T10:20:31.456Z","workspace_id":"workspace-1"}`

func TestRecordCanonicalEnvelope(t *testing.T) {
	t.Parallel()
	// Decode the declared wire contract so this first test runs before the new
	// body type exists; an old flat record silently loses every body field.
	const envelope = `{"body":` + recordCanonicalGolden + `,"signature":{"alg":"ed25519","kid":"fixture","sig":"AQ=="}}`
	var record MemoryRecord
	if err := json.Unmarshal([]byte(envelope), &record); err != nil {
		t.Fatal(err)
	}
	got, err := signing.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != envelope {
		t.Fatalf("MarshalCanonical(record envelope) = %s, want %s", got, envelope)
	}
	if record.Content != "remember <this>" {
		t.Errorf("record.Content = %q, want promoted body selector", record.Content)
	}
}

func recordSigningFixture() MemoryRecordBody {
	return MemoryRecordBody{
		ID: "record-1", Kind: KindWorking, Content: "remember <this>", Namespace: "notes", WorkspaceID: "workspace-1", SessionID: "session-visible",
		Provenance: Provenance{SourceKind: "conversation", SourceID: "source-1", Start: 2, End: 9, Hash: "sha256:fixture", OriginTool: "memory_create", OriginSessionID: "session-origin", TrustClass: TrustAgentWritten},
		Metadata:   json.RawMessage(`{ "z": 1e0, "a": {"z":1.0,"a":null} }`),
		CreatedAt:  time.Date(2026, 9, 5, 12, 20, 30, 123456789, time.FixedZone("fixture", 7200)),
		UpdatedAt:  time.Date(2026, 9, 5, 12, 20, 31, 456789123, time.FixedZone("fixture", 7200)),
		ExpiresAt:  time.Date(2026, 9, 6, 12, 20, 30, 789123456, time.FixedZone("fixture", 7200)),
	}
}

func recordTestSigner(t *testing.T) (*signing.Ed25519Signer, *signing.Keyring) {
	t.Helper()
	// Fixed synthetic test seed; never a runtime identity.
	signer, err := signing.NewEd25519Signer(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := signing.NewKeyring(signer.Verifier())
	if err != nil {
		t.Fatal(err)
	}
	return signer, ring
}

func TestRecordCanonicalBody(t *testing.T) {
	t.Parallel()
	body := recordSigningFixture()
	normalizeRecordTimes(&body)
	got, err := canonicalRecordBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != recordCanonicalGolden {
		t.Fatalf("canonicalRecordBody(fixture) = %s, want %s", got, recordCanonicalGolden)
	}
}

func TestRecordSignatureIndependent(t *testing.T) {
	t.Parallel()
	signer, ring := recordTestSigner(t)
	record := MemoryRecord{MemoryRecordBody: recordSigningFixture()}
	if err := signRecord(context.Background(), signer, &record); err != nil {
		t.Fatal(err)
	}
	if MemoryRecordDomain != "go-llm/memory-record/v1" {
		t.Fatalf("MemoryRecordDomain = %q", MemoryRecordDomain)
	}
	// Independent frame construction and stdlib verification over literal bytes.
	framed := []byte("go-llm-signing-v1\x00")
	framed = binary.BigEndian.AppendUint64(framed, uint64(len("go-llm/memory-record/v1")))
	framed = append(framed, "go-llm/memory-record/v1"...)
	framed = append(framed, recordCanonicalGolden...)
	pub := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, framed, record.Signature.Bytes) {
		t.Fatal("stdlib Verify(literal domain/body) = false")
	}
	const signatureHex = "a368c28c4cae200a5f883b6df7731859303d3db04f44957fa32061e02aa28c4ee8c76f302ed1caec10c41310b2e58016943100831871181a1ac041374c6dc40a"
	if got := hex.EncodeToString(record.Signature.Bytes); got != signatureHex {
		t.Errorf("signature = %s, want %s", got, signatureHex)
	}
	if err := verifyRecord(context.Background(), ring, record); err != nil {
		t.Fatal(err)
	}
	// Body canonical bytes never include the envelope's signature.
	record.Signature.Bytes = []byte("changed")
	body, err := canonicalRecordBody(record.MemoryRecordBody)
	if err != nil || string(body) != recordCanonicalGolden {
		t.Fatalf("canonical body after signature change = %s, %v", body, err)
	}
}

func TestRecordSignatureEveryField(t *testing.T) {
	t.Parallel()
	signer, ring := recordTestSigner(t)
	original := MemoryRecord{MemoryRecordBody: recordSigningFixture()}
	if err := signRecord(context.Background(), signer, &original); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*MemoryRecord){
		"id":                     func(r *MemoryRecord) { r.ID += "x" },
		"kind":                   func(r *MemoryRecord) { r.Kind = KindSemantic },
		"content":                func(r *MemoryRecord) { r.Content += "x" },
		"namespace":              func(r *MemoryRecord) { r.Namespace += "x" },
		"workspace":              func(r *MemoryRecord) { r.WorkspaceID += "x" },
		"session":                func(r *MemoryRecord) { r.SessionID += "x" },
		"source kind":            func(r *MemoryRecord) { r.Provenance.SourceKind += "x" },
		"source id":              func(r *MemoryRecord) { r.Provenance.SourceID += "x" },
		"source start":           func(r *MemoryRecord) { r.Provenance.Start++ },
		"source end":             func(r *MemoryRecord) { r.Provenance.End++ },
		"source hash":            func(r *MemoryRecord) { r.Provenance.Hash += "x" },
		"origin tool":            func(r *MemoryRecord) { r.Provenance.OriginTool += "x" },
		"origin session":         func(r *MemoryRecord) { r.Provenance.OriginSessionID += "x" },
		"trust":                  func(r *MemoryRecord) { r.Provenance.TrustClass = TrustLegacyUnreviewed },
		"metadata value":         func(r *MemoryRecord) { r.Metadata = json.RawMessage(`{"changed":true}`) },
		"metadata number lexeme": func(r *MemoryRecord) { r.Metadata = json.RawMessage(`{"a":{"a":null,"z":1},"z":1e0}`) },
		"created":                func(r *MemoryRecord) { r.CreatedAt = r.CreatedAt.Add(time.Millisecond) },
		"updated":                func(r *MemoryRecord) { r.UpdatedAt = r.UpdatedAt.Add(time.Millisecond) },
		"expires":                func(r *MemoryRecord) { r.ExpiresAt = r.ExpiresAt.Add(time.Millisecond) },
		"deleted":                func(r *MemoryRecord) { r.DeletedAt = time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			record := original
			mutate(&record)
			err := verifyRecord(context.Background(), ring, record)
			if !errors.Is(err, ErrRecordIntegrity) || !errors.Is(err, signing.ErrInvalidSignature) {
				t.Fatalf("verifyRecord(%s mutation) = %v, want integrity + invalid signature", name, err)
			}
		})
	}
}

func TestRecordCanonicalTimes(t *testing.T) {
	t.Parallel()
	monotonic := time.Now()
	cases := []struct {
		name        string
		input, want time.Time
	}{
		{"zero", time.Time{}, time.Time{}},
		{"epoch", time.Unix(0, 0), time.Time{}},
		{"sub-millisecond epoch", time.Unix(0, 999999), time.Time{}},
		{"before epoch", time.Unix(0, -1), time.UnixMilli(-1).UTC()},
		{"offset and fraction", time.Date(2026, 9, 5, 12, 20, 30, 123456789, time.FixedZone("fixture", 7200)), time.Date(2026, 9, 5, 10, 20, 30, 123000000, time.UTC)},
		{"monotonic", monotonic, time.UnixMilli(monotonic.UnixMilli()).UTC()},
	}
	signer, ring := recordTestSigner(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := recordSigningFixture()
			body.CreatedAt, body.UpdatedAt, body.ExpiresAt, body.DeletedAt = tc.input, tc.input, tc.input, tc.input
			record := MemoryRecord{MemoryRecordBody: body}
			if err := signRecord(context.Background(), signer, &record); err != nil {
				t.Fatal(err)
			}
			for name, got := range map[string]time.Time{"created": record.CreatedAt, "updated": record.UpdatedAt, "expires": record.ExpiresAt, "deleted": record.DeletedAt} {
				if got != tc.want {
					t.Errorf("signRecord(%s).%s = %#v, want %#v", tc.name, name, got, tc.want)
				}
			}
			// The storage representation uses four integer milliseconds. Restore each
			// exactly as scanRecord does, then normalize its UTC representation.
			stored := record
			stored.CreatedAt = fromMs(toMs(record.CreatedAt))
			stored.UpdatedAt = fromMs(toMs(record.UpdatedAt))
			stored.ExpiresAt = fromMs(toMs(record.ExpiresAt))
			stored.DeletedAt = fromMs(toMs(record.DeletedAt))
			normalizeRecordTimes(&stored.MemoryRecordBody)
			if !reflect.DeepEqual(stored, record) {
				t.Errorf("storage round trip(%s) changed signed record", tc.name)
			}
			if err := verifyRecord(context.Background(), ring, stored); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecordCanonicalBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, content string
		wantErr       bool
	}{
		{"exact bytes", strings.Repeat("x", 4096), false},
		{"over bytes", strings.Repeat("x", 4097), true},
		{"unicode bytes", strings.Repeat("é", 2048), false},
		{"unicode over bytes", strings.Repeat("é", 2049), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRecordContent(tc.content); (err != nil) != tc.wantErr {
				t.Errorf("validateRecordContent(%d bytes) = %v, want error %v", len(tc.content), err, tc.wantErr)
			}
		})
	}
	// Every variable-length field contributes independently. Empty content is
	// permitted here because content creation policy belongs to its own check.
	fields := map[string]func(*MemoryRecordBody, string){
		"id":             func(b *MemoryRecordBody, s string) { b.ID = s },
		"kind":           func(b *MemoryRecordBody, s string) { b.Kind = MemoryKind(s) },
		"content":        func(b *MemoryRecordBody, s string) { b.Content = s },
		"namespace":      func(b *MemoryRecordBody, s string) { b.Namespace = s },
		"workspace":      func(b *MemoryRecordBody, s string) { b.WorkspaceID = s },
		"session":        func(b *MemoryRecordBody, s string) { b.SessionID = s },
		"source kind":    func(b *MemoryRecordBody, s string) { b.Provenance.SourceKind = s },
		"source id":      func(b *MemoryRecordBody, s string) { b.Provenance.SourceID = s },
		"source hash":    func(b *MemoryRecordBody, s string) { b.Provenance.Hash = s },
		"origin tool":    func(b *MemoryRecordBody, s string) { b.Provenance.OriginTool = s },
		"origin session": func(b *MemoryRecordBody, s string) { b.Provenance.OriginSessionID = s },
		"metadata":       func(b *MemoryRecordBody, s string) { b.Metadata = json.RawMessage(`"` + s + `"`) },
	}
	for name, set := range fields {
		t.Run("aggregate/"+name, func(t *testing.T) {
			b := MemoryRecordBody{Provenance: Provenance{TrustClass: TrustAgentWritten}, Metadata: json.RawMessage(`{}`)}
			remaining := 32*1024 - len("agent-written") - 2
			set(&b, strings.Repeat("x", remaining))
			if _, err := canonicalRecordBody(b); err != nil {
				t.Fatalf("canonicalRecordBody(%s exactly 32KiB) = %v", name, err)
			}
			set(&b, strings.Repeat("x", remaining+1))
			if _, err := canonicalRecordBody(b); err == nil {
				t.Fatalf("canonicalRecordBody(%s over 32KiB) accepted", name)
			}
		})
	}
	// The read/sign path permits historically valid content above the write cap.
	b := recordSigningFixture()
	b.Content = strings.Repeat("x", 4097)
	if _, err := canonicalRecordBody(b); err != nil {
		t.Fatalf("canonicalRecordBody(legacy content) = %v", err)
	}
}

func TestRecordCanonicalRejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*MemoryRecordBody){
		"unknown trust":          func(b *MemoryRecordBody) { b.Provenance.TrustClass = "user-trusted" },
		"empty trust":            func(b *MemoryRecordBody) { b.Provenance.TrustClass = "" },
		"invalid string UTF8":    func(b *MemoryRecordBody) { b.Content = string([]byte{255}) },
		"nil metadata":           func(b *MemoryRecordBody) { b.Metadata = nil },
		"empty metadata":         func(b *MemoryRecordBody) { b.Metadata = json.RawMessage{} },
		"whitespace metadata":    func(b *MemoryRecordBody) { b.Metadata = json.RawMessage(" \t\n") },
		"invalid metadata":       func(b *MemoryRecordBody) { b.Metadata = json.RawMessage(`{"sensitive-source":`) },
		"duplicate decoded keys": func(b *MemoryRecordBody) { b.Metadata = json.RawMessage(`{"a":1,"\u0061":2}`) },
		"invalid metadata UTF8":  func(b *MemoryRecordBody) { b.Metadata = json.RawMessage{'"', 255, '"'} },
		"unpaired surrogate":     func(b *MemoryRecordBody) { b.Metadata = json.RawMessage(`"\ud800"`) },
	}
	signer, ring := recordTestSigner(t)
	original := MemoryRecord{MemoryRecordBody: recordSigningFixture()}
	if err := signRecord(context.Background(), signer, &original); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			record := original
			mutate(&record.MemoryRecordBody)
			if _, err := canonicalRecordBody(record.MemoryRecordBody); err == nil {
				t.Errorf("canonicalRecordBody(%s) accepted", name)
			}
			before := record
			err := verifyRecord(context.Background(), ring, record)
			if !errors.Is(err, ErrRecordIntegrity) {
				t.Errorf("verifyRecord(%s) = %v, want integrity error", name, err)
			}
			if strings.Contains(err.Error(), "sensitive-source") {
				t.Errorf("verifyRecord leaked parser payload: %v", err)
			}
			if !reflect.DeepEqual(before, record) {
				t.Errorf("verifyRecord(%s) repaired its input", name)
			}
		})
	}
}

func TestRecordSignatureMetadataNormalization(t *testing.T) {
	t.Parallel()
	signer, ring := recordTestSigner(t)
	for _, raw := range []json.RawMessage{nil, {}, []byte(" \t\n"), []byte("{}"), []byte("null")} {
		body := recordSigningFixture()
		norm, err := normalizeMetadata(raw)
		if err != nil {
			t.Fatal(err)
		}
		body.Metadata = json.RawMessage(norm)
		record := MemoryRecord{MemoryRecordBody: body}
		if err := signRecord(context.Background(), signer, &record); err != nil {
			t.Fatal(err)
		}
		want := "{}"
		if string(raw) == "null" {
			want = "null"
		}
		if string(record.Metadata) != want {
			t.Errorf("normalized metadata(%q) = %s, want %s", raw, record.Metadata, want)
		}
		if err := verifyRecord(context.Background(), ring, record); err != nil {
			t.Fatal(err)
		}
		// The same empty input may be normalized at authorized import/write, but
		// must never be normalized by the verification path.
		record.Metadata = nil
		if err := verifyRecord(context.Background(), ring, record); !errors.Is(err, ErrRecordIntegrity) {
			t.Errorf("verifyRecord(empty replacement) = %v", err)
		}
	}
	original := MemoryRecord{MemoryRecordBody: recordSigningFixture()}
	if err := signRecord(context.Background(), signer, &original); err != nil {
		t.Fatal(err)
	}
	original.Metadata = json.RawMessage(`{"a":{"z":1.0,"a":null},"z":1e0}`)
	if err := verifyRecord(context.Background(), ring, original); err != nil {
		t.Fatalf("metadata insignificant whitespace/key order: %v", err)
	}
}

func TestRecordSignatureErrors(t *testing.T) {
	t.Parallel()
	signer, ring := recordTestSigner(t)
	record := MemoryRecord{MemoryRecordBody: recordSigningFixture()}
	if err := signRecord(context.Background(), signer, &record); err != nil {
		t.Fatal(err)
	}
	missing := record
	missing.Signature = signing.Signature{}
	if err := verifyRecord(context.Background(), ring, missing); !errors.Is(err, ErrRecordIntegrity) || !errors.Is(err, errMissingRecordSignature) {
		t.Errorf("verifyRecord(missing) = %v", err)
	}
	unknown := record
	unknown.Signature.KeyID = "unknown"
	if err := verifyRecord(context.Background(), ring, unknown); !errors.Is(err, ErrRecordIntegrity) || !errors.Is(err, signing.ErrUnknownKey) {
		t.Errorf("verifyRecord(unknown key) = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifyRecord(ctx, ring, record); !errors.Is(err, context.Canceled) || errors.Is(err, ErrRecordIntegrity) {
		t.Errorf("verifyRecord(cancelled) = %v", err)
	}
	before := record
	if err := signRecord(ctx, signer, &record); !errors.Is(err, context.Canceled) {
		t.Errorf("signRecord(cancelled) = %v", err)
	}
	if !reflect.DeepEqual(before, record) {
		t.Error("failed sign mutated record")
	}
}

func TestRecordCanonicalMetadataWriteBoundaries(t *testing.T) {
	t.Parallel()
	store := newRecordStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{"nil", nil, "{}"}, {"empty", json.RawMessage{}, "{}"}, {"whitespace", json.RawMessage(" \t\n"), "{}"}, {"JSON null", json.RawMessage("null"), "null"},
	} {
		t.Run("create/"+tc.name, func(t *testing.T) {
			record, err := store.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "metadata fixture", Metadata: tc.raw})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.Get(ctx, record.ID, RecordAccess{})
			if err != nil {
				t.Fatal(err)
			}
			if string(record.Metadata) != tc.want || string(stored.Metadata) != tc.want {
				t.Errorf("Create(%s) metadata = %s / stored %s, want %s", tc.name, record.Metadata, stored.Metadata, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{"nil retains", nil, `{"keep":1}`}, {"empty replaces", json.RawMessage{}, "{}"}, {"whitespace replaces", json.RawMessage(" \t\n"), "{}"}, {"JSON null replaces", json.RawMessage("null"), "null"},
	} {
		t.Run("update/"+tc.name, func(t *testing.T) {
			record, err := store.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "metadata fixture", Metadata: json.RawMessage(`{"keep":1}`)})
			if err != nil {
				t.Fatal(err)
			}
			updated, err := store.Update(ctx, record.ID, RecordAccess{}, UpdateRecordParams{Metadata: tc.raw})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.Get(ctx, record.ID, RecordAccess{})
			if err != nil {
				t.Fatal(err)
			}
			if string(updated.Metadata) != tc.want || string(stored.Metadata) != tc.want {
				t.Errorf("Update(%s) metadata = %s / stored %s, want %s", tc.name, updated.Metadata, stored.Metadata, tc.want)
			}
		})
	}
}

func TestRecordCanonicalChecksSizeBeforeJSON(t *testing.T) {
	t.Parallel()
	body := recordSigningFixture()
	body.Metadata = json.RawMessage(strings.Repeat("x", 32*1024+1)) // oversized and invalid JSON
	if _, err := canonicalRecordBody(body); !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("canonicalRecordBody(oversized invalid JSON) = %v, want size error", err)
	}
	signer, ring := recordTestSigner(t)
	record := MemoryRecord{MemoryRecordBody: recordSigningFixture()}
	if err := signRecord(context.Background(), signer, &record); err != nil {
		t.Fatal(err)
	}
	record.Metadata = body.Metadata
	if err := verifyRecord(context.Background(), ring, record); !errors.Is(err, ErrRecordIntegrity) || !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("verifyRecord(oversized invalid JSON) = %v, want integrity + size", err)
	}
}
