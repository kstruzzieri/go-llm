package compat

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func closeBody(t testing.TB, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
}

type repeatByteReader struct {
	b byte
}

func (r repeatByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

func oversizedJSONStringReader(prefix string, n int64, suffix string) io.Reader {
	return io.MultiReader(
		strings.NewReader(prefix),
		io.LimitReader(repeatByteReader{b: 'a'}, n),
		strings.NewReader(suffix),
	)
}

func decodeErrorEnvelope(t testing.TB, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env
}
