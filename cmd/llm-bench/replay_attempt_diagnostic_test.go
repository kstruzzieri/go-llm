//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestReportAttemptDetailRedactsRawAndQuotedKey(t *testing.T) {
	const key = `lab-key-012345\"6789abcdef`
	var stderr bytes.Buffer
	reportAttemptDetail(&stderr, fmt.Errorf("raw %s; quoted %q", key, key), key)
	if want := "llm-bench: replay-attempt: raw [REDACTED]; quoted \"[REDACTED]\"\n"; stderr.String() != want {
		t.Fatalf("stderr %q, want %q", stderr.String(), want)
	}
}

func TestReplayAttempt_StderrRedactsQuotedToolName(t *testing.T) {
	cases := []struct{ name, key string }{
		{"quote", `lab-key-012345"6789abcdef`},
		{"backslash", `lab-key-012345\6789abcdef`},
		{"quote and backslash", `lab-key-012345\"6789abcdef`},
		{"leading backslash", `\lab-key-0123456789abcdef`},
		{"trailing backslash", `lab-key-0123456789abcdef\`},
		{"plain ASCII", "lab-key-0123456789abcdef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCompatServer(t, compatExchange{Body: compatToolCall(tc.key, `"not an object"`, "")})
			opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
			opts.APIKey = tc.key
			code, rec, stderr := runAttemptStderr(t, context.Background(), opts)
			if code != 0 {
				t.Fatalf("exit %d, want durable terminal", code)
			}
			assertAttemptError(t, rec, attemptCodeArgumentDecode, attemptStageReplay, 1)
			if received := srv.Received(); len(received) != 1 || received[0].Authorization != "Bearer "+tc.key {
				t.Fatal("candidate did not receive the accepted credential")
			}
			quotedKey := strconv.Quote(tc.key)
			for _, secret := range []string{tc.key, quotedKey[1 : len(quotedKey)-1]} {
				if strings.Contains(stderr, escapeControls(secret)) {
					t.Errorf("stderr retains a recoverable credential: %q", stderr)
				}
			}
			if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") || !strings.HasPrefix(stderr, "llm-bench: replay-attempt: ") {
				t.Errorf("stderr must remain one complete diagnostic line: %q", stderr)
			}
			if want := `openai-compat candidate tool "[REDACTED]" arguments: json: cannot unmarshal string`; !strings.Contains(stderr, want) {
				t.Errorf("stderr lost the argument-decode detail or redacted tool name: %q", stderr)
			}
		})
	}
}
