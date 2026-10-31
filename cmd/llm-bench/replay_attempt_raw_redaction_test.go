//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedactAttemptKeyPreservesRawEvidence(t *testing.T) {
	t.Parallel()
	unicodeKey := "lab_é🦊_0123456789"
	tests := []struct {
		name string
		key  string
		in   string
		want string
	}{
		{
			name: "mixed literal and unicode escapes",
			key:  publishTestKey,
			in:   ` { "echo" : "prefix lab_\u0030123456789abc\u0064ef suffix", "keep":"\u0061\/" } `,
			want: ` { "echo" : "prefix [REDACTED] suffix", "keep":"\u0061\/" } `,
		},
		{
			name: "all unicode escapes",
			key:  publishTestKey,
			in:   `{"echo":"` + escapeAttemptTestKey(publishTestKey) + `"}`,
			want: `{"echo":"[REDACTED]"}`,
		},
		{
			name: "duplicate members and escaped object key",
			key:  publishTestKey,
			in:   `{"same":1e+03,"same":2,"\u006cab_0123456789abcdef":"lab_0123456789abcdef","other":"\u0061"}`,
			want: `{"same":1e+03,"same":2,"[REDACTED]":"[REDACTED]","other":"\u0061"}`,
		},
		{
			name: "quote backslash and slash in credential",
			key:  `lab_"quote\slash/end`,
			in:   `{"echo":"before lab_\"quote\\slash\/end after","keep":"\u0061\/\\\""}`,
			want: `{"echo":"before [REDACTED] after","keep":"\u0061\/\\\""}`,
		},
		{
			name: "trailing backslash preserves closing quote",
			key:  `lab_0123456789abc\`,
			in:   `{"echo":"lab_0123456789abc\\","next":true}`,
			want: `{"echo":"[REDACTED]","next":true}`,
		},
		{
			name: "BMP unicode and surrogate pair with mixed hex case",
			key:  unicodeKey,
			in:   `{"echo":"lab_\u00E9\ud83E\uDD8A_0123456789","keep":"🦊"}`,
			want: `{"echo":"[REDACTED]","keep":"🦊"}`,
		},
		{
			name: "repeated and adjacent credentials",
			key:  publishTestKey,
			in:   `{"echo":"lab_0123456789abcdef\u006cab_0123456789abcdef","again":"lab_0123456789abcdef"}`,
			want: `{"echo":"[REDACTED][REDACTED]","again":"[REDACTED]"}`,
		},
		{
			name: "truncated final string retains incomplete escape",
			key:  publishTestKey,
			in:   `{"echo":"before lab_\u0030123456789abcdef after\u00`,
			want: `{"echo":"before [REDACTED] after\u00`,
		},
		{
			name: "malformed JSON preserves diagnostic bytes",
			key:  publishTestKey,
			in:   `not-json {"echo":"lab_\u0030123456789abcdef"} trailing ???`,
			want: `not-json {"echo":"[REDACTED]"} trailing ???`,
		},
		{
			name: "plain text and quoted text credentials",
			key:  publishTestKey,
			in:   `denied lab_0123456789abcdef; "lab_0123456789abcdef"`,
			want: `denied [REDACTED]; "[REDACTED]"`,
		},
		{
			name: "plain text credential crosses apparent quote boundaries",
			key:  `lab_"quote\slash/end`,
			in:   `denied lab_"quote\slash/end; retry later`,
			want: `denied [REDACTED]; retry later`,
		},
		{
			name: "plain text credential ends in backslash",
			key:  `lab_0123456789abc\`,
			in:   `denied lab_0123456789abc\; retry later`,
			want: `denied [REDACTED]; retry later`,
		},
		{
			name: "literal credential prefix of an escape retains byte redaction",
			key:  `lab_0123456789abc\`,
			in:   `{"echo":"lab_0123456789abc\n"}`,
			want: `{"echo":"[REDACTED]n"}`,
		},
		{
			name: "literal fallback never scans generated replacement bytes",
			key:  "[REDACTED]1234567",
			in:   `prefix "[REDACTED]12345671234567"`,
			want: `prefix "[REDACTED]1234567"`,
		},
		{
			name: "numeric credential retains literal byte redaction",
			key:  "1234567890123456",
			in:   `{"count":1234567890123456}`,
			want: `{"count":[REDACTED]}`,
		},
		{
			name: "invalid UTF8 credential retains literal redaction",
			key:  "lab_\xff_0123456789abcdef",
			in:   "{\"echo\":\"lab_\xff_0123456789abcdef\"}",
			want: `{"echo":"[REDACTED]"}`,
		},
		{
			name: "invalid UTF8 credential with trailing escaped backslash",
			key:  "lab_\xff_0123456789abc\\",
			in:   "{\"echo\":\"lab_\xff_0123456789abc\\\\\",\"next\":true}",
			want: `{"echo":"[REDACTED]","next":true}`,
		},
		{
			name: "unpaired surrogate follows JSON replacement semantics",
			key:  "lab_\ufffd_0123456789",
			in:   `{"echo":"lab_\ud800_0123456789","keep":"\ud800"}`,
			want: `{"echo":"[REDACTED]","keep":"\ud800"}`,
		},
		{
			name: "match after overlapping credential prefixes",
			key:  "aaaaaaaaaaaaaaab",
			in:   `{"echo":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaa\u0061b"}`,
			want: `{"echo":"aaaaaaaaaaaaaaa[REDACTED]"}`,
		},
		{
			name: "credential free evidence remains byte identical",
			key:  publishTestKey,
			in:   " {\n\t\"same\":1e+03,\"same\":2,\"keep\":\"\\u0061\\/\" } ",
			want: " {\n\t\"same\":1e+03,\"same\":2,\"keep\":\"\\u0061\\/\" } ",
		},
		{
			name: "escaped literal backslash is not a unicode escape",
			key:  publishTestKey,
			in:   `{"echo":"lab_\\u0030123456789abcdef"}`,
			want: `{"echo":"lab_\\u0030123456789abcdef"}`,
		},
		{
			name: "short credential is unchanged even when escaped",
			key:  "short",
			in:   `{"echo":"\u0073hort short"}`,
			want: `{"echo":"\u0073hort short"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			original := []byte(tt.in)
			got, redacted := redactAttemptKey(original, tt.key)
			if string(got) != tt.want || redacted != (tt.in != tt.want) {
				t.Errorf("redactAttemptKey(%q, %q) = (%q, %v), want (%q, %v)", tt.in, tt.key, got, redacted, tt.want, tt.in != tt.want)
			}
			if string(original) != tt.in {
				t.Errorf("redactAttemptKey changed its input to %q, want %q", original, tt.in)
			}
		})
	}
}

func escapeAttemptTestKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

func TestAttemptPublisherEscapedKeyReferencesStoredBytes(t *testing.T) {
	t.Parallel()
	dir := claimTemp(t)
	request := []byte(`{"echo":"lab_\u0030123456789abcdef","same":1,"same":2}`)
	response := []byte(`{"echo":"` + escapeAttemptTestKey(publishTestKey) + ` after`)
	rows := []*recordedRequest{{Ordinal: 1, RequestBody: request, HeadersAt: time.Now(), ResponseBody: response, ResponseComplete: false}}
	refs, aerr, causes := newAttemptPublisher(dir).writeRaw(rows, publishTestKey)
	if aerr != nil || len(causes) != 0 {
		t.Fatalf("writeRaw escaped credentials: error %+v, causes %v", aerr, causes)
	}
	if refs[0].Request == nil || refs[0].Response == nil {
		t.Fatalf("writeRaw escaped credentials: refs %+v, want both evidence files", refs[0])
	}
	checks := []struct {
		ref  *attemptFileRef
		want []byte
	}{
		{refs[0].Request, []byte(`{"echo":"[REDACTED]","same":1,"same":2}`)},
		{&refs[0].Response.attemptFileRef, []byte(`{"echo":"[REDACTED] after`)},
	}
	for _, check := range checks {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(check.ref.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, check.want) || !check.ref.Redacted || check.ref.Bytes != len(got) || check.ref.SHA256 != sha256Hex(got) {
			t.Errorf("writeRaw stored %q with ref %+v, want %q with redacted=true and matching length/hash", got, check.ref, check.want)
		}
	}
	if refs[0].Response.Complete {
		t.Error("writeRaw truncated response: Complete=true, want false")
	}
}
