// Package brokenbody serves a reply whose body breaks after its headers, for
// tests of how the HTTP clients read a failed body. Only tests import it.
package brokenbody

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Fixture pairs the broken reply with a client that reports when the reply's
// headers reached it, so a reset waits for the headers instead of a sleep.
type Fixture struct {
	headers chan struct{}
	once    sync.Once
}

// New returns a fixture for one broken reply.
func New() *Fixture {
	return &Fixture{headers: make(chan struct{})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Client returns an HTTP client, on a transport of its own, that signals the
// fixture when a response's headers have been returned to it.
func (f *Fixture) Client() *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err == nil {
			f.once.Do(func() { close(f.headers) })
		}
		return resp, err
	})}
}

// headerWait bounds how long Write waits for the client; reaching it means
// the test's client did not come from Client, and Write reports it.
const headerWait = 5 * time.Second

// Write hijacks w and sends status with a Content-Length of 4096 and a body
// cut short. Without reset it then closes the connection: a short body. With
// reset it waits until the headers reached the fixture's client, then resets
// the connection, so the reset fails the body read and cannot discard the
// headers.
func (f *Fixture) Write(w http.ResponseWriter, status int, reset bool) error {
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return fmt.Errorf("brokenbody: hijack: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"error\":\"cut", status, http.StatusText(status))
	if err := buf.Flush(); err != nil {
		return fmt.Errorf("brokenbody: write: %w", err)
	}
	if !reset {
		return nil
	}
	select {
	case <-f.headers:
	case <-time.After(headerWait):
		return errors.New("brokenbody: the headers never reached the fixture's client")
	}
	return conn.(*net.TCPConn).SetLinger(0)
}
