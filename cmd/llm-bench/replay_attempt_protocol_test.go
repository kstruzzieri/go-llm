//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplayAttempt_UpgradePublishesBeforePeerCloses(t *testing.T) {
	upgraded := make(chan error, 1)
	releasePeer := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upgraded <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_, err = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		if err == nil {
			err = rw.Flush()
		}
		upgraded <- err
		<-releasePeer
	}))
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	opts.Timeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer
	var exitCode int
	done := make(chan struct{})
	go func() {
		exitCode = runReplayAttempt(ctx, opts, &stderr)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		close(releasePeer)
		srv.Close()
		<-done // Closing the peer also releases the pre-fix blocked read.
	})
	select {
	case err := <-upgraded:
		if err != nil {
			t.Fatalf("upgrade server: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive an upgrade request")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("attempt blocked beyond its timeout while the upgraded peer stayed open")
	}
	if exitCode != 0 {
		t.Fatalf("exit %d, stderr %q; want a published terminal", exitCode, stderr.String())
	}
	rec := readTerminal(t, opts.AttemptDir)
	assertAttemptError(t, rec, attemptCodeHTTPStatus, attemptStageResponse, 1)
	if rec.Error.HTTPStatus != 101 || len(rec.Requests) != 1 || len(rec.Transcript) != 0 {
		t.Fatalf("upgrade record status=%+v requests=%d transcript=%+v", rec.Error, len(rec.Requests), rec.Transcript)
	}
	row := rec.Requests[0]
	if row.HTTPStatus == nil || *row.HTTPStatus != 101 || row.Response == nil || row.Response.Complete {
		t.Fatalf("upgrade row %+v; want status 101 and incomplete response evidence", row)
	}
	raw, err := os.ReadFile(filepath.Join(opts.AttemptDir, filepath.FromSlash(row.Response.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("response evidence %q; upgraded protocol bytes must never be read", raw)
	}
}

func TestReplayAttempt_RejectsRepeatedChoicesBeforeProviderDecode(t *testing.T) {
	cases := []struct{ name, body string }{
		{"partial message retains content", `{"choices":[{"message":{"role":"assistant","content":"stale"}}],"choices":[{"message":{}}]}`},
		{"null choice retains content", `{"choices":[{"message":{"role":"assistant","content":"stale"}}],"choices":[null]}`},
		{"null content retains content", `{"choices":[{"message":{"role":"assistant","content":"stale"}}],"choices":[{"message":{"content":null}}]}`},
		{"escaped duplicate retains content", `{"choices":[{"message":{"role":"assistant","content":"stale"}}],"cho\u0069ces":[{"message":{}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCompatServer(t, compatExchange{Body: tc.body})
			opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
			code, rec := runAttempt(t, context.Background(), opts)
			if code != 0 {
				t.Fatalf("exit %d; want a published terminal", code)
			}
			assertAttemptError(t, rec, attemptCodeResponseDecode, attemptStageResponse, 1)
			assertOneSequence(t, opts, srv, rec)
			if len(rec.Transcript) != 0 || len(rec.Requests) != 1 {
				t.Fatalf("transcript=%+v requests=%d; rejected choices must not advance replay", rec.Transcript, len(rec.Requests))
			}
			row := rec.Requests[0]
			if row.ErrorCode == nil || *row.ErrorCode != attemptCodeResponseDecode || row.Response == nil || !row.Response.Complete {
				t.Fatalf("row %+v; want response_decode with complete response evidence", row)
			}
			raw, err := os.ReadFile(filepath.Join(opts.AttemptDir, filepath.FromSlash(row.Response.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.body || row.Response.SHA256 != sha256Hex([]byte(tc.body)) {
				t.Fatalf("response evidence %q, sha256=%q; want the complete rejected body", raw, row.Response.SHA256)
			}
		})
	}
}
