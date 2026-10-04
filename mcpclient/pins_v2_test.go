package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeV1Pin(t *testing.T, s *PinStore, alias string, c toolCatalog) []byte {
	t.Helper()
	raw, err := json.Marshal(pinRecord{Version: catalogFormatVersion, Workspace: s.workspace, Alias: alias, Digest: c.digest(), Tools: c.canonicalBytes()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, pinKey(alias)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPinV1RecordIsConnectionMissing(t *testing.T) {
	ctx := context.Background()
	s := pinStoreForTest(t)
	a := pinCatalog(t, "fs", "A")
	raw := writeV1Pin(t, s, "fs", a.toolCatalog)
	got, rev, err := s.capturePin(ctx, "fs")
	if err != nil || !rev.exists || got.conn != nil || got.digest() != a.digest() {
		t.Fatalf("v1 capture = (%v, %v, %v), want catalog with no connection", got.conn, rev.exists, err)
	}
	for _, candidate := range []pinEntry{a, pinCatalog(t, "fs", "changed")} {
		for _, require := range []bool{false, true} {
			if _, created, err := s.admit(ctx, "fs", candidate, require); !errors.Is(err, errConnectionMissing) || created {
				t.Fatalf("admit over v1 (require=%t) = (%t, %v), want errConnectionMissing", require, created, err)
			}
		}
	}
	if !bytes.Equal(raw, readPinBytes(t, s)) {
		t.Fatal("v1 record rewritten without approval")
	}
}

func TestPinAdmitRevalidatesConnection(t *testing.T) {
	ctx := context.Background()
	s := pinStoreForTest(t)
	if _, _, err := s.admit(ctx, "fs", pinCatalog(t, "fs", "A"), false); err != nil {
		t.Fatal(err)
	}
	other := pinCatalog(t, "fs", "A")
	other.conn = testConnPin()
	other.conn.fingerprint = "hmac-sha256:" + strings.Repeat("e", 64)
	other.conn.fields["endpoint"] = strings.Repeat("f", 64)
	_, _, err := s.admit(ctx, "fs", other, false)
	var changed *connectionChangedError
	if !errors.As(err, &changed) || !slices.Equal(changed.labels, []string{"endpoint"}) {
		t.Fatalf("admit with changed connection = %v, want connection change [endpoint]", err)
	}
}

func TestPinAdmitAtRevision(t *testing.T) {
	ctx := context.Background()
	s := pinStoreForTest(t)
	a := pinCatalog(t, "fs", "A")
	_, absent, err := s.capturePin(ctx, "fs")
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.admitAt(ctx, "fs", absent, a); err != nil || !created {
		t.Fatalf("first admitAt = (%t, %v), want created", created, err)
	}
	if _, created, err := s.admitAt(ctx, "fs", absent, a); !errors.Is(err, errPinRevisionConflict) || created {
		t.Fatalf("stale absent revision = (%t, %v), want errPinRevisionConflict", created, err)
	}
	_, present, err := s.capturePin(ctx, "fs")
	if err != nil {
		t.Fatal(err)
	}
	// An operator deleting the pin between preflight and admission must not
	// see it silently re-created.
	if err := os.Remove(filepath.Join(s.dir, fsKey+".json")); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.admitAt(ctx, "fs", present, a); !errors.Is(err, errPinRevisionConflict) || created {
		t.Fatalf("admitAt after deletion = (%t, %v), want errPinRevisionConflict", created, err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, fsKey+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted pin was re-created: %v", err)
	}
	// Bytes that no longer decode are still a changed revision: admission and
	// approval report a conflict, not an invalid pin, and rewrite nothing.
	corrupt := []byte("corrupt")
	if err := os.WriteFile(filepath.Join(s.dir, fsKey+".json"), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.admitAt(ctx, "fs", present, a); !errors.Is(err, errPinRevisionConflict) || created {
		t.Fatalf("admitAt over undecodable bytes = (%t, %v), want errPinRevisionConflict", created, err)
	}
	if err := s.replacePin(ctx, "fs", present, a); !errors.Is(err, errPinRevisionConflict) {
		t.Fatalf("replacePin over undecodable bytes = %v, want errPinRevisionConflict", err)
	}
	if !bytes.Equal(corrupt, readPinBytes(t, s)) {
		t.Fatal("undecodable record rewritten")
	}
}

func TestPinRecordV2Strictness(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any, map[string]any){
		"extra top-level":       func(r, _ map[string]any) { r["extra"] = true },
		"missing connection":    func(r, _ map[string]any) { delete(r, "connection") },
		"null connection":       func(r, _ map[string]any) { r["connection"] = nil },
		"extra connection key":  func(_, c map[string]any) { c["extra"] = "x" },
		"missing key_id":        func(_, c map[string]any) { delete(c, "key_id") },
		"null fields":           func(_, c map[string]any) { c["fields"] = nil },
		"extra field":           func(_, c map[string]any) { c["fields"].(map[string]any)["argv"] = strings.Repeat("0", 64) },
		"missing field":         func(_, c map[string]any) { delete(c["fields"].(map[string]any), "origin") },
		"catalog-style digest":  func(_, c map[string]any) { c["fingerprint"] = "sha256:" + strings.Repeat("a", 64) },
		"uppercase hex":         func(_, c map[string]any) { c["key_id"] = strings.Repeat("B", 64) },
		"unknown kind":          func(_, c map[string]any) { c["kind"] = "ftp" },
		"kind field mismatch":   func(_, c map[string]any) { c["kind"] = "stdio" },
		"null field value":      func(_, c map[string]any) { c["fields"].(map[string]any)["origin"] = nil },
		"unsupported version 3": func(r, _ map[string]any) { r["version"] = 3 },
		"case-variant key":      func(_, c map[string]any) { c["Fingerprint"] = c["fingerprint"]; delete(c, "fingerprint") },
		"case-variant extra":    func(_, c map[string]any) { c["Fingerprint"] = c["fingerprint"] },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := pinStoreForTest(t)
			if _, _, err := s.admit(ctx, "fs", pinCatalog(t, "fs", "A"), false); err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(readPinBytes(t, s), &record); err != nil {
				t.Fatal(err)
			}
			mutate(record, record["connection"].(map[string]any))
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.dir, fsKey+".json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.capturePin(ctx, "fs"); err == nil {
				t.Fatalf("%s accepted", name)
			}
			if !bytes.Equal(raw, readPinBytes(t, s)) {
				t.Fatal("invalid record rewritten")
			}
		})
	}
}

func TestPinRecordStoresNoIdentityValues(t *testing.T) {
	pins := testPins(t)
	s, _, _ := staticCatalogServer(t, "fs", tool("read"))
	s.command = []string{"/canary-launcher", "--token=canary-argv"}
	s.dir = "/canary-dir"
	s.env = []EnvVar{SetEnv("CANARY_NAME", "canary-value")}
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if len(m.Tools()) != 1 {
		t.Fatalf("not admitted: %v", w)
	}
	raw := pinBytes(t, pins, "fs")
	if bytes.Contains(raw, []byte("canary")) || bytes.Contains(raw, []byte("CANARY")) || !bytes.Contains(raw, []byte(`"version":2`)) {
		t.Fatalf("pin record = %s; want version 2 with no identity values", raw)
	}
}
