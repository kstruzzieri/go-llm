package signing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
)

type fakeVerifier struct {
	kid string
	alg Algorithm
}

const goldenDomain = "go-llm/test/v1"

var goldenPayload = []byte(`{"a":1}`)

func (f fakeVerifier) KeyID() string        { return f.kid }
func (f fakeVerifier) Algorithm() Algorithm { return f.alg }
func (f fakeVerifier) Verify(context.Context, string, []byte, Signature) error {
	return nil
}

func TestFrameGolden(t *testing.T) {
	got, err := frame("d", []byte("p"))
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	const want = "676f2d6c6c6d2d7369676e696e672d76310000000000000000016470"
	if hex.EncodeToString(got) != want {
		t.Fatalf("frame(d,p) = %x\nwant %s", got, want)
	}
}

func TestFrameTestVectorGolden(t *testing.T) {
	got, err := frame("go-llm/test/v1", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	const want = "676f2d6c6c6d2d7369676e696e672d763100000000000000000e676f2d6c6c6d2f746573742f76317b2261223a317d"
	if hex.EncodeToString(got) != want {
		t.Fatalf("frame = %x\nwant %s", got, want)
	}
	if len(got) != 47 {
		t.Fatalf("frame len = %d, want 47", len(got))
	}
}

func TestFrameLengthPrefixDisambiguates(t *testing.T) {
	a, err := frame("ab", []byte("c"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := frame("a", []byte("bc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatalf("frame(ab,c) == frame(a,bc): domain boundary is ambiguous")
	}
}

func TestFrameRejectsEmptyDomain(t *testing.T) {
	if _, err := frame("", []byte("p")); !errors.Is(err, ErrEmptyDomain) {
		t.Fatalf("err = %v, want ErrEmptyDomain", err)
	}
}

func TestSignatureJSONGolden(t *testing.T) {
	sig := Signature{Alg: AlgEd25519, KeyID: "k", Bytes: []byte{1, 2, 3}}
	b, err := json.Marshal(sig)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"alg":"ed25519","kid":"k","sig":"AQID"}`
	if string(b) != want {
		t.Fatalf("json = %s\nwant %s", b, want)
	}
	var back Signature
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Alg != sig.Alg || back.KeyID != sig.KeyID || !bytes.Equal(back.Bytes, sig.Bytes) {
		t.Fatalf("round trip = %+v, want %+v", back, sig)
	}
}

func TestCheckBinding(t *testing.T) {
	v := fakeVerifier{kid: "k1", alg: AlgHMACSHA256}
	cases := []struct {
		name string
		sig  Signature
		want error
	}{
		{"match", Signature{Alg: AlgHMACSHA256, KeyID: "k1"}, nil},
		{"wrong kid", Signature{Alg: AlgHMACSHA256, KeyID: "k2"}, ErrKeyMismatch},
		{"wrong alg", Signature{Alg: AlgEd25519, KeyID: "k1"}, ErrKeyMismatch},
		{"empty", Signature{}, ErrKeyMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkBinding(v, tc.sig); !errors.Is(err, tc.want) {
				t.Fatalf("checkBinding = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDeriveKeyIDGoldenAndAlgorithmBinding(t *testing.T) {
	material := []byte("same-key-material")
	const wantEd = "2f6c7fad6ae597f9ca27633c7b88c84edd7b68752035b804841e311481efa57d"
	const wantHMAC = "4029f15864e16d574c63aa1afd4d97724872e1e4414e3b1105108d1f225b071d"
	gotEd := deriveKeyID(AlgEd25519, material)
	gotHMAC := deriveKeyID(AlgHMACSHA256, material)
	if gotEd != wantEd {
		t.Fatalf("ed25519 kid = %s, want %s", gotEd, wantEd)
	}
	if gotHMAC != wantHMAC {
		t.Fatalf("hmac kid = %s, want %s", gotHMAC, wantHMAC)
	}
	if gotEd == gotHMAC {
		t.Fatal("key id does not bind the algorithm")
	}
}
