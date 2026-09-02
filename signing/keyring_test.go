package signing

import (
	"context"
	"errors"
	"slices"
	"testing"
)

type pointerVerifier struct{ id string }

func (v *pointerVerifier) KeyID() string        { return v.id }
func (v *pointerVerifier) Algorithm() Algorithm { return AlgEd25519 }
func (v *pointerVerifier) Verify(context.Context, string, []byte, Signature) error {
	return nil
}

func TestKeyringRotation(t *testing.T) {
	ctx := context.Background()
	const domain = "go-llm/keyring/v1"
	old, err := GenerateEd25519(nil)
	if err != nil {
		t.Fatal(err)
	}
	oldSig, err := old.Sign(ctx, domain, []byte("old record"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := GenerateEd25519(nil)
	if err != nil {
		t.Fatal(err)
	}
	newSig, err := current.Sign(ctx, domain, []byte("new record"))
	if err != nil {
		t.Fatal(err)
	}
	kr, err := NewKeyring(old.Verifier(), current.Verifier())
	if err != nil {
		t.Fatal(err)
	}
	if err := kr.Verify(ctx, domain, []byte("old record"), oldSig); err != nil {
		t.Fatalf("old key must keep verifying: %v", err)
	}
	if err := kr.Verify(ctx, domain, []byte("new record"), newSig); err != nil {
		t.Fatalf("current key: %v", err)
	}
	swapped := oldSig
	swapped.KeyID = current.KeyID()
	if err := kr.Verify(ctx, domain, []byte("old record"), swapped); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("kid rewritten to another ring member: err = %v, want ErrInvalidSignature", err)
	}
	stranger, err := GenerateEd25519(nil)
	if err != nil {
		t.Fatal(err)
	}
	ssig, err := stranger.Sign(ctx, domain, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := kr.Verify(ctx, domain, []byte("x"), ssig); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown kid: err = %v, want ErrUnknownKey", err)
	}
}

func TestKeyringMixedBackends(t *testing.T) {
	ctx := context.Background()
	e, err := GenerateEd25519(nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := GenerateHMAC(nil)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := NewKeyring(e.Verifier(), h)
	if err != nil {
		t.Fatal(err)
	}
	es, err := e.Sign(ctx, "d", []byte("p"))
	if err != nil {
		t.Fatal(err)
	}
	hs, err := h.Sign(ctx, "d", []byte("p"))
	if err != nil {
		t.Fatal(err)
	}
	if err := kr.Verify(ctx, "d", []byte("p"), es); err != nil {
		t.Fatal(err)
	}
	if err := kr.Verify(ctx, "d", []byte("p"), hs); err != nil {
		t.Fatal(err)
	}
}

func TestKeyringAddRejects(t *testing.T) {
	var kr Keyring // zero value is usable
	e, _ := GenerateEd25519(nil)
	if err := kr.Add(e.Verifier()); err != nil {
		t.Fatal(err)
	}
	if err := kr.Add(e.Verifier()); err == nil {
		t.Fatal("duplicate kid accepted")
	}
	if err := kr.Add(nil); err == nil {
		t.Fatal("nil verifier accepted")
	}
	var typedNil *pointerVerifier
	if err := kr.Add(typedNil); err == nil {
		t.Fatal("typed-nil verifier accepted")
	}
	if err := kr.Add(&Ed25519Verifier{}); err == nil {
		t.Fatal("zero-value verifier accepted")
	}
	if err := kr.Add(fakeVerifier{kid: "", alg: AlgEd25519}); err == nil {
		t.Fatal("empty kid accepted")
	}
	if err := kr.Add(fakeVerifier{kid: "nonempty", alg: ""}); err == nil {
		t.Fatal("empty algorithm accepted")
	}
	if _, err := NewKeyring(e.Verifier(), e.Verifier()); err == nil {
		t.Fatal("NewKeyring accepted duplicates")
	}
}

func TestKeyringKeyIDsSorted(t *testing.T) {
	var vs []Verifier
	for i := 0; i < 5; i++ {
		s, _ := GenerateEd25519(nil)
		vs = append(vs, s.Verifier())
	}
	kr, err := NewKeyring(vs...)
	if err != nil {
		t.Fatal(err)
	}
	ids := kr.KeyIDs()
	if len(ids) != 5 {
		t.Fatalf("len = %d", len(ids))
	}
	if !slices.IsSorted(ids) {
		t.Fatalf("not sorted: %v", ids)
	}
	for _, v := range vs {
		if !slices.Contains(ids, v.KeyID()) {
			t.Fatalf("missing %s", v.KeyID())
		}
	}
}
