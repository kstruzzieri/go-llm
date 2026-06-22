package tools

import (
	"strings"
	"testing"
)

func TestContentHashDistinctFromAbsent(t *testing.T) {
	if contentHash([]byte("")) == absentHash {
		t.Fatal("hash of empty content must differ from the absent sentinel")
	}
	if contentHash([]byte("a")) == contentHash([]byte("b")) {
		t.Fatal("different content must hash differently")
	}
	h1 := contentHash([]byte("a"))
	h2 := contentHash([]byte("a"))
	if h1 != h2 {
		t.Fatal("hash must be stable")
	}
}

func TestUnifiedDiffNewFile(t *testing.T) {
	d := unifiedDiff("a.txt", nil, []byte("one\ntwo\n"), false)
	if !strings.Contains(d, "new file: a.txt") || !strings.Contains(d, "+one") || !strings.Contains(d, "+two") {
		t.Fatalf("new-file diff wrong:\n%s", d)
	}
}

func TestUnifiedDiffEmpty(t *testing.T) {
	d := unifiedDiff("a.txt", []byte("one\ntwo\n"), nil, true)
	if !strings.Contains(d, "empty file: a.txt") || !strings.Contains(d, "-one") {
		t.Fatalf("empty diff wrong:\n%s", d)
	}
}

func TestUnifiedDiffChangeTrimsCommon(t *testing.T) {
	before := []byte("ctx1\nold\nctx2\n")
	after := []byte("ctx1\nnew\nctx2\n")
	d := unifiedDiff("a.txt", before, after, true)
	if !strings.Contains(d, "-old") || !strings.Contains(d, "+new") {
		t.Fatalf("change diff missing -/+ lines:\n%s", d)
	}
	if strings.Contains(d, "-ctx1") || strings.Contains(d, "+ctx1") {
		t.Fatalf("common prefix must not be marked changed:\n%s", d)
	}
}

func TestPendingPlanStoreConsumeByArgsHash(t *testing.T) {
	var base mutatingBase
	pp := pendingPlan{path: "a.txt", afterContent: []byte("x")}
	base.store("HASH1", pp)
	if _, ok := base.consume("WRONG"); ok {
		t.Fatal("consume with wrong hash must fail")
	}
	got, ok := base.consume("HASH1")
	if !ok || got.path != "a.txt" {
		t.Fatalf("consume HASH1: ok=%v got=%+v", ok, got)
	}
	if _, ok := base.consume("HASH1"); ok {
		t.Fatal("second consume must fail (plan cleared)")
	}
}

func TestUnifiedDiffIdenticalNoChangeLines(t *testing.T) {
	d := unifiedDiff("a.txt", []byte("x\ny\n"), []byte("x\ny\n"), true)
	for _, ln := range strings.Split(d, "\n") {
		if strings.HasPrefix(ln, "+") || strings.HasPrefix(ln, "-") {
			// the +++/--- header lines are allowed; only single +/- change lines are not
			if !strings.HasPrefix(ln, "+++") && !strings.HasPrefix(ln, "---") {
				t.Fatalf("identical inputs produced a change line: %q\nfull:\n%s", ln, d)
			}
		}
	}
}
