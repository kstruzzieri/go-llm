package signing

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// backends is the table every future backend (SSH agent, KMS) joins. Each
// entry must produce a fresh, independent key per call.
var backends = []struct {
	name string
	new  func(t *testing.T) (Signer, Verifier)
}{
	{"ed25519", func(t *testing.T) (Signer, Verifier) {
		t.Helper()
		s, err := GenerateEd25519(nil)
		if err != nil {
			t.Fatal(err)
		}
		return s, s.Verifier()
	}},
	{"hmac-sha256", func(t *testing.T) (Signer, Verifier) {
		t.Helper()
		s, err := GenerateHMAC(nil)
		if err != nil {
			t.Fatal(err)
		}
		return s, s
	}},
}

func mustVerify(t *testing.T, v Verifier, domain string, payload []byte, sig Signature, what string) {
	t.Helper()
	if err := v.Verify(context.Background(), domain, payload, sig); err != nil {
		t.Fatalf("%s: verify = %v, want nil", what, err)
	}
}

func mustFail(t *testing.T, v Verifier, domain string, payload []byte, sig Signature, want error, what string) {
	t.Helper()
	if err := v.Verify(context.Background(), domain, payload, sig); !errors.Is(err, want) {
		t.Errorf("%s: verify = %v, want %v", what, err, want)
	}
}

func TestConformance(t *testing.T) {
	const domain = "go-llm/conformance/v1"
	payload := []byte(`{"k":"v"}`)
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			s, v := b.new(t)

			if len(s.KeyID()) != 64 {
				t.Fatalf("KeyID %q is not 64 hex chars", s.KeyID())
			}
			sig, err := s.Sign(ctx, domain, payload)
			if err != nil {
				t.Fatal(err)
			}
			if sig.Alg != s.Algorithm() || sig.KeyID != s.KeyID() {
				t.Fatalf("signature binding %s/%s, signer %s/%s", sig.Alg, sig.KeyID, s.Algorithm(), s.KeyID())
			}
			if len(sig.Bytes) == 0 {
				t.Fatal("empty signature bytes")
			}
			mustVerify(t, v, domain, payload, sig, "round trip")

			esig, err := s.Sign(ctx, domain, nil)
			if err != nil {
				t.Fatal(err)
			}
			mustVerify(t, v, domain, nil, esig, "empty payload")
			mustVerify(t, v, domain, []byte{}, esig, "empty payload (non-nil)")

			if _, err := s.Sign(ctx, "", payload); !errors.Is(err, ErrEmptyDomain) {
				t.Errorf("sign empty domain = %v, want ErrEmptyDomain", err)
			}
			mustFail(t, v, "", payload, sig, ErrEmptyDomain, "verify empty domain")

			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := s.Sign(cancelled, domain, payload); !errors.Is(err, context.Canceled) {
				t.Errorf("sign cancelled context = %v, want context.Canceled", err)
			}
			if err := v.Verify(cancelled, domain, payload, sig); !errors.Is(err, context.Canceled) {
				t.Errorf("verify cancelled context = %v, want context.Canceled", err)
			}

			for i := range payload {
				for bit := 0; bit < 8; bit++ {
					p := bytes.Clone(payload)
					p[i] ^= 1 << bit
					mustFail(t, v, domain, p, sig, ErrInvalidSignature, "payload flip")
				}
			}
			for i := range sig.Bytes {
				m := sig
				m.Bytes = bytes.Clone(sig.Bytes)
				m.Bytes[i] ^= 0x01
				mustFail(t, v, domain, payload, m, ErrInvalidSignature, "signature byte flip")
			}
			mustFail(t, v, domain, append(bytes.Clone(payload), ' '), sig, ErrInvalidSignature, "payload appended")
			mustFail(t, v, domain+"x", payload, sig, ErrInvalidSignature, "domain appended")
			mustFail(t, v, "go-llm/conformance/v2", payload, sig, ErrInvalidSignature, "domain version bumped")
			mustFail(t, v, "go-llm/conformance/v", append([]byte("1"), payload...), sig, ErrInvalidSignature, "domain/payload boundary shifted")

			m := sig
			m.KeyID = strings.Repeat("0", 64)
			mustFail(t, v, domain, payload, m, ErrKeyMismatch, "kid forge")
			m = sig
			m.Alg = "none"
			mustFail(t, v, domain, payload, m, ErrKeyMismatch, "alg none")
			m = sig
			m.Bytes = sig.Bytes[:len(sig.Bytes)-1]
			mustFail(t, v, domain, payload, m, ErrInvalidSignature, "truncated")
			m = sig
			m.Bytes = append(bytes.Clone(sig.Bytes), 0)
			mustFail(t, v, domain, payload, m, ErrInvalidSignature, "extended")
			m = sig
			m.Bytes = nil
			mustFail(t, v, domain, payload, m, ErrInvalidSignature, "nil bytes")

			other, _ := b.new(t)
			if other.KeyID() == s.KeyID() {
				t.Fatal("two generated keys share a KeyID")
			}
			osig, err := other.Sign(ctx, domain, payload)
			if err != nil {
				t.Fatal(err)
			}
			mustFail(t, v, domain, payload, osig, ErrKeyMismatch, "other key, honest kid")
			forged := osig
			forged.KeyID = s.KeyID()
			mustFail(t, v, domain, payload, forged, ErrInvalidSignature, "other key, forged kid")
		})
	}
}

func TestConformanceCrossBackendConfusion(t *testing.T) {
	ctx := context.Background()
	const domain = "go-llm/conformance/v1"
	payload := []byte(`{"k":"v"}`)
	e, err := GenerateEd25519(nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := GenerateHMAC(nil)
	if err != nil {
		t.Fatal(err)
	}
	hsig, err := h.Sign(ctx, domain, payload)
	if err != nil {
		t.Fatal(err)
	}
	esig, err := e.Sign(ctx, domain, payload)
	if err != nil {
		t.Fatal(err)
	}

	f := hsig
	f.KeyID = e.KeyID()
	mustFail(t, e.Verifier(), domain, payload, f, ErrKeyMismatch, "hmac tag to ed25519, alg honest")
	f.Alg = AlgEd25519
	mustFail(t, e.Verifier(), domain, payload, f, ErrInvalidSignature, "hmac tag to ed25519, alg forged")

	g := esig
	g.KeyID = h.KeyID()
	mustFail(t, h, domain, payload, g, ErrKeyMismatch, "ed25519 sig to hmac, alg honest")
	g.Alg = AlgHMACSHA256
	mustFail(t, h, domain, payload, g, ErrInvalidSignature, "ed25519 sig to hmac, alg forged")
}

func TestConformanceConcurrentUse(t *testing.T) {
	// doc.go: signers and verifiers are immutable after construction and safe
	// for concurrent use. Run under the package's -race gate.
	const domain = "go-llm/conformance/v1"
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			s, v := b.new(t)
			var wg sync.WaitGroup
			errs := make(chan error, 8*100)
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					payload := []byte{byte(g)}
					for i := 0; i < 100; i++ {
						sig, err := s.Sign(context.Background(), domain, payload)
						if err == nil {
							err = v.Verify(context.Background(), domain, payload, sig)
						}
						if err != nil {
							errs <- err
							return
						}
					}
				}(g)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
		})
	}
}
