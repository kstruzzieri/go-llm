package signing

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"
)

const (
	goldenHMACKID = "5f62d964e17c603a4b0adaa3d813f1f682d1ba2cba5c963307c27ab5adf43a9d"
	goldenHMACTag = "c0b9473297d0df1f63333d3e183c9059105bd938013f969dea77c3633abd6232"
)

func goldenHMACKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

func TestHMACGolden(t *testing.T) {
	ctx := context.Background()
	m, err := NewHMAC(goldenHMACKey())
	if err != nil {
		t.Fatal(err)
	}
	if m.KeyID() != goldenHMACKID {
		t.Fatalf("kid = %s, want %s", m.KeyID(), goldenHMACKID)
	}
	if m.Algorithm() != AlgHMACSHA256 {
		t.Fatalf("alg = %s", m.Algorithm())
	}
	sig, err := m.Sign(ctx, goldenDomain, goldenPayload)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sig.Bytes); got != goldenHMACTag {
		t.Fatalf("tag = %s\nwant %s", got, goldenHMACTag)
	}
	if sig.Alg != AlgHMACSHA256 || sig.KeyID != goldenHMACKID {
		t.Fatalf("sig binding = %s/%s", sig.Alg, sig.KeyID)
	}
	if err := m.Verify(ctx, goldenDomain, goldenPayload, sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestHMACRejectsShortKey(t *testing.T) {
	if _, err := NewHMAC(make([]byte, 31)); err == nil { // literal, not MinHMACKeySize-1
		t.Fatal("31-byte key accepted")
	}
	if _, err := NewHMAC(nil); err == nil {
		t.Fatal("nil key accepted")
	}
	if _, err := NewHMAC(make([]byte, 64)); err != nil {
		t.Fatalf("64-byte key rejected: %v", err)
	}
}

func TestHMACGenerateAndClone(t *testing.T) {
	m, err := GenerateHMAC(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.KeyID()) != 64 {
		t.Fatalf("kid %q is not 64 hex chars", m.KeyID())
	}
	key := goldenHMACKey()
	m2, err := NewHMAC(key)
	if err != nil {
		t.Fatal(err)
	}
	key[0] ^= 0xff // caller mutates its copy after construction
	sig, err := m2.Sign(context.Background(), goldenDomain, goldenPayload)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(sig.Bytes) != goldenHMACTag {
		t.Fatal("HMACSigner did not clone the key: tag changed after caller mutation")
	}
}

func TestHMACRedaction(t *testing.T) {
	m, err := NewHMAC(goldenHMACKey())
	if err != nil {
		t.Fatal(err)
	}
	assertNoKeyMaterial(t, m, goldenHMACKey())
}

func TestHMACZeroValueFailsClosed(t *testing.T) {
	ctx := context.Background()
	var m HMACSigner
	if m.KeyID() != "" {
		t.Fatal("zero-value HMAC signer exposed a key id")
	}
	if _, err := m.Sign(ctx, goldenDomain, goldenPayload); !errors.Is(err, ErrUninitializedKey) {
		t.Fatalf("zero-value sign err = %v, want ErrUninitializedKey", err)
	}
	if err := m.Verify(ctx, goldenDomain, goldenPayload, Signature{}); !errors.Is(err, ErrUninitializedKey) {
		t.Fatalf("zero-value verify err = %v, want ErrUninitializedKey", err)
	}
}

func TestHMACRedactionOfValueShapes(t *testing.T) {
	m, err := NewHMAC(goldenHMACKey())
	if err != nil {
		t.Fatal(err)
	}
	shapes := []any{
		*m, struct{ H HMACSigner }{*m}, &struct{ H HMACSigner }{*m},
		[]HMACSigner{*m}, [1]HMACSigner{*m}, map[string]HMACSigner{"k": *m}, any(*m),
	}
	assertNoKeyMaterialShapes(t, m.KeyID(), shapes, goldenHMACKey())
}

func TestGenerateHMACUsesSuppliedReader(t *testing.T) {
	key := goldenHMACKey()
	got, err := GenerateHMAC(bytes.NewReader(key))
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID() != goldenHMACKID {
		t.Fatalf("GenerateHMAC ignored the supplied reader: kid %s, want %s", got.KeyID(), goldenHMACKID)
	}
	if _, err := GenerateHMAC(bytes.NewReader(key[:10])); err == nil {
		t.Fatal("short reader accepted")
	}
}

func TestHMACNilContextFailsClosed(t *testing.T) {
	var nilCtx context.Context
	m, err := NewHMAC(goldenHMACKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Sign(nilCtx, goldenDomain, goldenPayload); err == nil {
		t.Fatal("nil context accepted by Sign")
	}
	if err := m.Verify(nilCtx, goldenDomain, goldenPayload, Signature{}); err == nil {
		t.Fatal("nil context accepted by Verify")
	}
}
