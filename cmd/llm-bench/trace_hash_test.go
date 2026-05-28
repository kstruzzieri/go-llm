package main

import (
	"strings"
	"testing"
)

func TestCanonicalTraceHashIgnoresIDDifferences(t *testing.T) {
	a := Trace{ID: "trace-A", Source: "test", Turns: []Turn{{Role: "user", Content: "hi"}}}
	b := Trace{ID: "trace-B", Source: "test", Turns: []Turn{{Role: "user", Content: "hi"}}}
	if got, want := canonicalTraceHash(a), canonicalTraceHash(b); got != want {
		t.Fatalf("canonicalTraceHash should ignore ID; got %s != %s", got, want)
	}
}

func TestCanonicalTraceHashReactsToContent(t *testing.T) {
	a := Trace{ID: "x", Turns: []Turn{{Role: "user", Content: "hi"}}}
	b := Trace{ID: "x", Turns: []Turn{{Role: "user", Content: "hello"}}}
	if canonicalTraceHash(a) == canonicalTraceHash(b) {
		t.Fatalf("canonicalTraceHash must react to Turn content changes")
	}
}

func TestTraceSetManifestHashOrderInvariant(t *testing.T) {
	t1 := Trace{ID: "t1", Turns: []Turn{{Role: "user", Content: "a"}}}
	t2 := Trace{ID: "t2", Turns: []Turn{{Role: "user", Content: "b"}}}
	h12 := traceSetManifestHash([]Trace{t1, t2})
	h21 := traceSetManifestHash([]Trace{t2, t1})
	if h12 != h21 {
		t.Fatalf("traceSetManifestHash must be order-invariant; got %s != %s", h12, h21)
	}
}

func TestTraceSetManifestHashContentSensitive(t *testing.T) {
	t1a := Trace{ID: "t1", Turns: []Turn{{Role: "user", Content: "a"}}}
	t1b := Trace{ID: "t1", Turns: []Turn{{Role: "user", Content: "DIFFERENT"}}}
	if traceSetManifestHash([]Trace{t1a}) == traceSetManifestHash([]Trace{t1b}) {
		t.Fatalf("same ID, different content must produce different manifest hash")
	}
}

func TestTraceSetManifestHashEmpty(t *testing.T) {
	got := traceSetManifestHash(nil)
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("manifest hash should be prefixed with sha256:; got %q", got)
	}
}

func TestTraceSetManifestHashSingleTrace(t *testing.T) {
	t1 := Trace{ID: "t1", Turns: []Turn{{Role: "user", Content: "a"}}}
	got := traceSetManifestHash([]Trace{t1})
	if len(got) != len("sha256:")+64 {
		t.Fatalf("expected sha256:<64 hex>; got len=%d", len(got))
	}
}
