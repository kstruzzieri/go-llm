package responsebound

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// countingBody records the bytes read through it and whether it was closed.
type countingBody struct {
	r      io.Reader
	read   int64
	closes int
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *countingBody) Close() error {
	b.closes++
	return nil
}

func TestResponseBound_UnboundedContextLeavesBodyAlone(t *testing.T) {
	ctx := context.Background()
	if Bounded(ctx) {
		t.Fatal("Bounded(Background) = true")
	}
	body := &countingBody{r: strings.NewReader("x")}
	if got := Wrap(ctx, body); got != io.ReadCloser(body) {
		t.Fatalf("Wrap on an unbounded context returned %T, want the body unchanged", got)
	}
	hc := &http.Client{}
	if got := HTTPClient(ctx, hc); got != hc {
		t.Fatal("HTTPClient on an unbounded context returned a copy")
	}
	if !Bounded(WithLimit(ctx, 10)) {
		t.Fatal("Bounded(WithLimit) = false")
	}
}

func TestResponseBound_ExactLimitReadsToEOF(t *testing.T) {
	const limit = 1000
	body := &countingBody{r: bytes.NewReader(bytes.Repeat([]byte("a"), limit))}
	got, err := io.ReadAll(Wrap(WithLimit(context.Background(), limit), body))
	if err != nil {
		t.Fatalf("ReadAll at exactly the limit: %v", err)
	}
	if len(got) != limit {
		t.Fatalf("read %d bytes, want %d", len(got), limit)
	}
	if body.closes != 0 {
		t.Fatalf("body closed %d times on a clean end, want 0", body.closes)
	}
}

func TestResponseBound_OverflowReadsLimitPlusOneAndCloses(t *testing.T) {
	const limit = 1000
	for _, size := range []int{limit + 1, 10 * limit} {
		body := &countingBody{r: bytes.NewReader(bytes.Repeat([]byte("a"), size))}
		wrapped := Wrap(WithLimit(context.Background(), limit), body)
		got, err := io.ReadAll(wrapped)
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("size %d: err = %v, want ErrExceeded", size, err)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("size %d: overflow reads as an end of body: %v", size, err)
		}
		if len(got) > limit {
			t.Fatalf("size %d: delivered %d bytes, want at most %d", size, len(got), limit)
		}
		if body.read > limit+1 {
			t.Fatalf("size %d: read %d bytes from the body, want at most %d", size, body.read, limit+1)
		}
		if body.closes != 1 {
			t.Fatalf("size %d: body closed %d times on overflow, want 1", size, body.closes)
		}
		if _, err := wrapped.Read(make([]byte, 1)); !errors.Is(err, ErrExceeded) {
			t.Fatalf("size %d: read after overflow = %v, want ErrExceeded", size, err)
		}
		if err := wrapped.Close(); err != nil || body.closes != 1 {
			t.Fatalf("size %d: Close after overflow = %v with %d closes, want nil and 1", size, err, body.closes)
		}
	}
}

func TestResponseBound_ErrExceededTextIsFixed(t *testing.T) {
	if strings.Contains(ErrExceeded.Error(), "%") || ErrExceeded.Error() == "" {
		t.Fatalf("ErrExceeded = %q", ErrExceeded)
	}
}

func TestResponseBound_RejectionReadError(t *testing.T) {
	cause := io.ErrUnexpectedEOF
	var err error = &RejectionReadError{StatusCode: 403, Err: cause}
	if !errors.Is(err, cause) {
		t.Fatal("RejectionReadError does not unwrap to its cause")
	}
	text := err.Error()
	if !strings.HasPrefix(text, "responsebound: ") || !strings.Contains(text, "403") || !strings.Contains(text, cause.Error()) {
		t.Fatalf("Error() = %q, want the fixed prefix, the status and the cause", text)
	}
	if _, ok := err.(interface{ HTTPStatusCode() int }); ok {
		t.Fatal("RejectionReadError has HTTPStatusCode(); a status-keyed rule would match it")
	}
	var hs interface{ HTTPStatusCode() int }
	if errors.As(err, &hs) {
		t.Fatal("errors.As finds an HTTPStatusCode() through RejectionReadError")
	}
}

func TestResponseBound_HTTPClientRefusesRedirects(t *testing.T) {
	ctx := WithLimit(context.Background(), 10)
	denied := errors.New("denied by policy")
	tests := []struct {
		name   string
		policy func(*http.Request, []*http.Request) error
		want   error
	}{
		{"no policy", nil, http.ErrUseLastResponse},
		{"permissive policy", func(*http.Request, []*http.Request) error { return nil }, http.ErrUseLastResponse},
		{"refusing policy", func(*http.Request, []*http.Request) error { return denied }, denied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := &http.Client{CheckRedirect: tt.policy, Transport: http.DefaultTransport}
			got := HTTPClient(ctx, hc)
			if got == hc {
				t.Fatal("HTTPClient returned the shared client for a bounded request")
			}
			if got.Transport != hc.Transport {
				t.Fatal("the copy does not share the transport")
			}
			if err := got.CheckRedirect(nil, nil); !errors.Is(err, tt.want) {
				t.Fatalf("CheckRedirect = %v, want %v", err, tt.want)
			}
			if tt.policy == nil && hc.CheckRedirect != nil {
				t.Fatal("the shared client's policy was changed")
			}
		})
	}
}
