//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An escaped credential echoed as a tool-call ID reaches both the response
// artifact and the following request. Redaction must change stored evidence
// without changing the conversation sent to the candidate or breaking JSON.
func TestReplayAttempt_RedactsJSONEncodedKeyAcrossRequests(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		encoded string
	}{
		{"unicode escape", attemptTestKey, `\u006cab-attempt-key-0123456789`},
		{"quote", `lab-quote"key-0123456789`, `lab-quote\"key-0123456789`},
		{"trailing backslash", `lab-key-0123456789\`, `lab-key-0123456789\\`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quoted, err := json.Marshal(tc.key)
			if err != nil {
				t.Fatal(err)
			}
			body := strings.Replace(compatToolCallID(tc.key, "rag_search", `{"query":"retry budget"}`, ""), string(quoted), `"`+tc.encoded+`"`, 1)
			srv := newCompatServer(t,
				compatExchange{Body: body},
				compatExchange{Body: compatText("done", "")},
			)
			opts := attemptTestOptions(t, srv.URL, fixtureToolRouteTrace)
			opts.APIKey = tc.key
			code, rec := runAttempt(t, context.Background(), opts)
			if code != 0 || rec.Status != attemptStatusCompleted || len(rec.Requests) != 2 {
				t.Fatalf("exit %d status %q error %+v requests %d", code, rec.Status, rec.Error, len(rec.Requests))
			}
			if len(rec.Transcript) != 3 || rec.Transcript[1].ToolCallID != attemptRedacted {
				t.Fatal("terminal transcript retained the echoed credential")
			}
			for _, row := range rec.Requests {
				if row.Request == nil || row.Response == nil {
					t.Fatalf("request %d is missing raw evidence references", row.Ordinal)
				}
			}
			received := srv.Received()
			if len(received) != 2 || !strings.Contains(string(received[1].Body), string(quoted)) {
				t.Fatal("the next wire request must carry the original tool-call ID")
			}
			for _, check := range []struct {
				ref  *attemptFileRef
				want string
			}{
				{&rec.Requests[0].Response.attemptFileRef, strings.ReplaceAll(body, tc.encoded, attemptRedacted)},
				{rec.Requests[1].Request, strings.ReplaceAll(string(received[1].Body), string(quoted), `"[REDACTED]"`)},
			} {
				stored, err := os.ReadFile(filepath.Join(opts.AttemptDir, filepath.FromSlash(check.ref.Path)))
				if err != nil {
					t.Fatal(err)
				}
				if string(stored) != check.want || !json.Valid(stored) {
					t.Fatalf("%s: stored %q, want %q as valid JSON", check.ref.Path, stored, check.want)
				}
				if !check.ref.Redacted || check.ref.Bytes != len(stored) || check.ref.SHA256 != sha256Hex(stored) {
					t.Fatalf("ref %+v does not describe the redacted stored bytes", check.ref)
				}
			}
			if rec.Requests[0].Request.Redacted || rec.Requests[1].Response.Redacted {
				t.Fatal("key-free artifacts must not be marked redacted")
			}
		})
	}
}
