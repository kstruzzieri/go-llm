package signing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
)

const (
	rfc8032Seed = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	rfc8032Pub  = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
	goldenKID   = "b0f0e4099cd739f05a2defb07e1940a08ffabcfc8ce64a4e6deeaa9365b4bf00"
	goldenSig   = "ab48bbf15f75a5828515356bff04c71c09392fbb3ed4bf5276c8f1efec9128c6ce0c236fe07e8c383114969e941417bde7820a3d0291cd48349568595a207607"
	goldenJSON  = `{"alg":"ed25519","kid":"b0f0e4099cd739f05a2defb07e1940a08ffabcfc8ce64a4e6deeaa9365b4bf00","sig":"q0i78V91pYKFFTVr/wTHHAk5L7s+1L9Sdsjx7+yRKMbODCNv4H6MODEUlp6UFBe954IKPQKRzUg0lWhZWiB2Bw=="}`
)

func goldenEd25519(t *testing.T) *Ed25519Signer {
	t.Helper()
	seed, err := hex.DecodeString(rfc8032Seed)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewEd25519Signer(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEd25519Golden(t *testing.T) {
	ctx := context.Background()
	s := goldenEd25519(t)
	if got := hex.EncodeToString(s.PublicKey()); got != rfc8032Pub {
		t.Fatalf("pub = %s, want %s", got, rfc8032Pub)
	}
	if s.KeyID() != goldenKID {
		t.Fatalf("kid = %s, want %s", s.KeyID(), goldenKID)
	}
	if s.Algorithm() != AlgEd25519 {
		t.Fatalf("alg = %s", s.Algorithm())
	}
	sig, err := s.Sign(ctx, goldenDomain, goldenPayload)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sig.Bytes); got != goldenSig {
		t.Fatalf("sig = %s\nwant %s", got, goldenSig)
	}
	j, err := json.Marshal(sig)
	if err != nil {
		t.Fatal(err)
	}
	if string(j) != goldenJSON {
		t.Fatalf("json = %s\nwant %s", j, goldenJSON)
	}
	v, err := NewEd25519Verifier(s.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if v.KeyID() != goldenKID {
		t.Fatalf("verifier kid = %s", v.KeyID())
	}
	if err := v.Verify(ctx, goldenDomain, goldenPayload, sig); err != nil {
		t.Fatalf("verifier verify: %v", err)
	}
	if err := s.Verifier().Verify(ctx, goldenDomain, goldenPayload, sig); err != nil {
		t.Fatalf("Verifier() verify: %v", err)
	}
}

func TestEd25519ConstructorsRejectBadLengths(t *testing.T) {
	pub, _ := hex.DecodeString(rfc8032Pub)
	if _, err := NewEd25519Verifier(pub[:31]); err == nil {
		t.Fatal("31-byte public key accepted")
	}
	if _, err := NewEd25519Verifier(append(pub, 0)); err == nil {
		t.Fatal("33-byte public key accepted")
	}
	if _, err := NewEd25519Signer(make([]byte, 63)); err == nil {
		t.Fatal("63-byte private key accepted")
	}
	if _, err := NewEd25519Signer(nil); err == nil {
		t.Fatal("nil private key accepted")
	}
	seed, _ := hex.DecodeString(rfc8032Seed)
	bad := ed25519.NewKeyFromSeed(seed)
	bad[ed25519.SeedSize] ^= 0x01
	if _, err := NewEd25519Signer(bad); err == nil {
		t.Fatal("private key with a public half inconsistent with its seed accepted")
	}
}

func TestEd25519VerifyRejectsWrongLengthSignature(t *testing.T) {
	ctx := context.Background()
	s := goldenEd25519(t)
	sig, err := s.Sign(ctx, goldenDomain, goldenPayload)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 63, 65} {
		m := sig
		m.Bytes = make([]byte, n)
		copy(m.Bytes, sig.Bytes)
		if err := s.Verifier().Verify(ctx, goldenDomain, goldenPayload, m); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("len %d: err = %v, want ErrInvalidSignature", n, err)
		}
	}
}

func TestEd25519ZeroValueFailsClosed(t *testing.T) {
	ctx := context.Background()
	var signer Ed25519Signer
	if signer.KeyID() != "" || signer.PublicKey() != nil || signer.Verifier() != nil {
		t.Fatal("zero-value signer exposed key state")
	}
	if _, err := signer.Sign(ctx, goldenDomain, goldenPayload); !errors.Is(err, ErrUninitializedKey) {
		t.Fatalf("zero-value signer err = %v, want ErrUninitializedKey", err)
	}
	var verifier Ed25519Verifier
	if verifier.KeyID() != "" || verifier.PublicKey() != nil {
		t.Fatal("zero-value verifier exposed key state")
	}
	if err := verifier.Verify(ctx, goldenDomain, goldenPayload, Signature{}); !errors.Is(err, ErrUninitializedKey) {
		t.Fatalf("zero-value verifier err = %v, want ErrUninitializedKey", err)
	}
}

func TestEd25519GenerateAndClone(t *testing.T) {
	s, err := GenerateEd25519(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.KeyID()) != 64 {
		t.Fatalf("kid %q is not 64 hex chars", s.KeyID())
	}
	pub := s.PublicKey()
	pub[0] ^= 0xff
	if s.PublicKey()[0] == pub[0] {
		t.Fatal("PublicKey() returned the internal slice")
	}
	verifierInput := s.PublicKey()
	wantFirst := verifierInput[0]
	v, err := NewEd25519Verifier(verifierInput)
	if err != nil {
		t.Fatal(err)
	}
	verifierInput[0] ^= 0xff
	if v.PublicKey()[0] != wantFirst {
		t.Fatal("verifier did not clone its public-key input")
	}
	verifierOutput := v.PublicKey()
	verifierOutput[0] ^= 0xff
	if v.PublicKey()[0] == verifierOutput[0] {
		t.Fatal("verifier PublicKey() returned the internal slice")
	}
	priv := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	copy(priv, ed25519.NewKeyFromSeed(make([]byte, 32)))
	s2, err := NewEd25519Signer(priv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before, err := s2.Sign(ctx, goldenDomain, goldenPayload)
	if err != nil {
		t.Fatal(err)
	}
	priv[0] ^= 0xff // caller mutates its copy after construction
	after, err := s2.Sign(ctx, goldenDomain, goldenPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Bytes, after.Bytes) {
		t.Fatal("signer did not clone the private key: signature changed after caller mutation")
	}
}

func TestEd25519Redaction(t *testing.T) {
	seed, _ := hex.DecodeString(rfc8032Seed)
	s := goldenEd25519(t)
	priv := ed25519.NewKeyFromSeed(seed)
	assertNoKeyMaterial(t, s, seed, priv)
	assertNoKeyMaterial(t, s.Verifier(), seed, priv)
}
