// Package responsebound marks a request as a bounded request. Only the
// capability probers create one. For a bounded request, and for no other, the
// HTTP clients count decoded response bytes against a limit, refuse every
// redirect, and report a rejection whose body could not be read as a read
// failure that carries the status separately. The mark travels on the request
// context, so a shared client is never changed.
package responsebound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrExceeded reports that a bounded response passed its byte limit. Its text
// is fixed and never carries provider text.
var ErrExceeded = errors.New("responsebound: response exceeded the byte limit")

type limitKey struct{}

// WithLimit returns ctx marked as a bounded request whose decoded response may
// be at most maxBytes long. A negative limit reads as zero.
func WithLimit(ctx context.Context, maxBytes int64) context.Context {
	return context.WithValue(ctx, limitKey{}, max(maxBytes, 0))
}

// Bounded reports whether ctx marks a bounded request.
func Bounded(ctx context.Context) bool {
	_, ok := ctx.Value(limitKey{}).(int64)
	return ok
}

// Wrap returns body counted against ctx's limit, or body unchanged when ctx
// is not bounded. The wrapper reads at most the limit plus one detection
// byte; on passing the limit it closes body and returns ErrExceeded, never an
// end of file that could pass for a normal end.
func Wrap(ctx context.Context, body io.ReadCloser) io.ReadCloser {
	limit, ok := ctx.Value(limitKey{}).(int64)
	if !ok {
		return body
	}
	return &boundedBody{body: body, limit: limit}
}

// boundedBody is used by one reader at a time, as a response body is.
type boundedBody struct {
	body     io.ReadCloser
	limit    int64
	read     int64
	exceeded bool
	closed   bool
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.exceeded {
		return 0, ErrExceeded
	}
	if room := b.limit + 1 - b.read; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := b.body.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		b.exceeded = true
		_ = b.Close()
		return n - int(b.read-b.limit), ErrExceeded
	}
	return n, err
}

// Close closes the underlying body once.
func (b *boundedBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	return b.body.Close()
}

// HTTPClient returns hc unchanged when ctx is not bounded. For a bounded
// request it returns a copy of hc, sharing its Transport and so its connection
// pool, whose redirect policy runs hc's own policy first and returns its error
// if it gives one, and otherwise hands back the redirect response unfollowed.
// The redirect target receives nothing.
func HTTPClient(ctx context.Context, hc *http.Client) *http.Client {
	if !Bounded(ctx) {
		return hc
	}
	refusing := *hc
	policy := hc.CheckRedirect
	refusing.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if policy != nil {
			if err := policy(req, via); err != nil {
				return err
			}
		}
		return http.ErrUseLastResponse
	}
	return &refusing
}

// RejectionReadError is a bounded request's non-2xx reply whose body could not
// be read: the status arrived, the body then failed. It deliberately has no
// HTTPStatusCode method, so no status-keyed rule matches it by accident; a
// caller that wants the status asks for it with errors.As.
type RejectionReadError struct {
	StatusCode int
	Err        error // the read failure, already redacted by the client
}

// Error returns a fixed prefix, the status code and the cause; never provider
// text.
func (e *RejectionReadError) Error() string {
	return fmt.Sprintf("responsebound: status %d rejection body unreadable: %v", e.StatusCode, e.Err)
}

// Unwrap returns the read failure.
func (e *RejectionReadError) Unwrap() error { return e.Err }
