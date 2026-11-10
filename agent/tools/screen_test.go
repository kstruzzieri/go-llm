package tools

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

const (
	tM = "[withheld: credential-path content (credential_path)]"
	tP = "[output withheld from here on: a patch of a credential-path file begins (credential_path); rerun as a direct argv call that excludes the file to see the rest, for example: git log -p -- . ':(exclude,glob)**/.env']"
	tL = "[output withheld from here on: a line over 32768 bytes cannot be screened (credential_path)]"
)

func screenRun(keys []string, chunks ...string) string {
	var kb [][]byte
	for _, k := range keys {
		kb = append(kb, []byte(k))
	}
	var buf bytes.Buffer
	w := newOutputScreen(kb).stream(&buf)
	for _, c := range chunks {
		n, err := w.Write([]byte(c))
		if n != len(c) || err != nil {
			return "SHORT WRITE"
		}
	}
	w.finish()
	return buf.String()
}

func TestScreenStream(t *testing.T) {
	longKey := "value-0001-abcdef"
	diff := "a\ndiff --git a/.env b/.env\n--- a/.env\n+++ b/.env\n+NAME=x\ndiff --git a/x b/x\n+later\n"
	diffD := strings.ReplaceAll(diff, "\n+", "\nd")
	rows := []struct {
		label string
		keys  []string
		in    string
		want  string
		bytes bool // also check one byte at a time
	}{
		{"plain", nil, "hello\nworld\n", "hello\nworld\n", false},
		{"color", nil, "\x1b[32mok\x1b[m\n", "\x1b[32mok\x1b[m\n", false},
		{"crlf", nil, "a\r\nb\r\n", "a\r\nb\r\n", false},
		{"progress", nil, "10%\r20%\r", "10%\r20%\r", false},
		{"marker text", nil, tM + "\n" + tP + "\n", tM + "\n" + tP + "\n", false},
		{"span", []string{longKey}, "x NAME=value-0001-abcdef y\n", "x NAME=" + tM + " y\n", true},
		{"span csi", []string{longKey}, "NAME=value-\x1b[01;31m\x1b[K0001\x1b[m\x1b[K-abcdef\n", "NAME=" + tM + "\n", true},
		{"span raw", []string{longKey}, "\x00\xff value-0001-abcdef\n", "\x00\xff " + tM + "\n", true},
		{"longest first", []string{"value-0001", longKey}, "x value-0001-abcdef y\n", "x " + tM + " y\n", true},
		{"line", nil, ".env:3:NAME=short\nnext\n", tM + "\nnext\n", true},
		{"cut", nil, diff, "a\n" + tP + "\n", true},
		{"cut d", nil, diffD, "a\n" + tP + "\n", true},
		{"cut and line", nil, "diff --git a/.env b/.env:text\n", tP + "\n", true},
		{"cut key", []string{"zzzz-key-1"}, "diff --git a/.env b/.env zzzz-key-1\n", tP + "\n", true},
		{"go results", nil, "    --- FAIL: TestNames/.env (0.00s)\n        x_test.go:9: detail\n",
			"    --- FAIL: TestNames/.env (0.00s)\n        x_test.go:9: detail\n", false},
		{"bound ok", nil, strings.Repeat("a", 32768) + "\n", strings.Repeat("a", 32768) + "\n", false},
		{"long header", nil, "diff --git " + strings.Repeat("a", 33000) + "/.env\n+NAME=x\n", tL + "\n", false},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			if got := screenRun(r.keys, r.in); got != r.want {
				t.Fatalf("one write: got %d bytes, want %d", len(got), len(r.want))
			}
			if !r.bytes {
				return
			}
			chunks := strings.Split(r.in, "")
			if got := screenRun(r.keys, chunks...); got != r.want {
				t.Fatalf("byte-wise: got %d bytes, want %d", len(got), len(r.want))
			}
		})
	}

	t.Run("holding", func(t *testing.T) {
		var buf bytes.Buffer
		w := newOutputScreen(nil).stream(&buf)
		_, _ = w.Write([]byte("partial"))
		if buf.Len() != 0 {
			t.Fatalf("held record delivered early: %d", buf.Len())
		}
		w.finish()
		w.finish()
		if buf.String() != "partial" {
			t.Fatalf("after finish: %d bytes", buf.Len())
		}
		_, _ = w.Write([]byte("more\n"))
		if buf.String() != "partial" {
			t.Fatalf("write after finish delivered")
		}
		buf.Reset()
		w = newOutputScreen(nil).stream(&buf)
		_, _ = w.Write([]byte("partial\r"))
		if buf.String() != "partial\r" {
			t.Fatalf("cr record held")
		}
	})

	t.Run("hold bound", func(t *testing.T) {
		var buf bytes.Buffer
		w := newOutputScreen(nil).stream(&buf)
		_, _ = w.Write([]byte(strings.Repeat("a", 32769)))
		if buf.String() != tL+"\n" {
			t.Fatalf("after first write: %d bytes", buf.Len())
		}
		if w.held != nil {
			t.Fatalf("held buffer kept after cut: %d", len(w.held))
		}
		_, _ = w.Write([]byte("\nshort\n"))
		w.finish()
		if buf.String() != tL+"\n" {
			t.Fatalf("later output delivered: %d bytes", buf.Len())
		}
	})

	t.Run("after release", func(t *testing.T) {
		for _, keys := range [][]string{{"value-0001-abcdef"}, nil} {
			var kb [][]byte
			for _, k := range keys {
				kb = append(kb, []byte(k))
			}
			var buf bytes.Buffer
			s := newOutputScreen(kb)
			w := s.stream(&buf)
			s.release()
			n, err := w.Write([]byte("x NAME=value-0001-abcdef y\n"))
			w.finish()
			if n != 27 || err != nil || buf.Len() != 0 {
				t.Fatalf("keys=%d n=%d err=%v out=%d", len(keys), n, err, buf.Len())
			}
		}
		var buf bytes.Buffer
		s := newOutputScreen(nil)
		w := s.stream(&buf)
		_, _ = w.Write([]byte("partial"))
		s.release()
		w.finish()
		if buf.Len() != 0 {
			t.Fatalf("held record delivered after release: %d", buf.Len())
		}
	})

	t.Run("after cut discards held", func(t *testing.T) {
		got := screenRun(nil, "diff --git a/.env b/.env\n", "held")
		if got != tP+"\n" {
			t.Fatalf("got %d bytes", len(got))
		}
	})

	t.Run("two streams", func(t *testing.T) {
		var a, b bytes.Buffer
		s := newOutputScreen(nil)
		wa, wb := s.stream(&a), s.stream(&b)
		_, _ = wa.Write([]byte("+++ b/.env\n"))
		_, _ = wb.Write([]byte("fine\n"))
		wa.finish()
		wb.finish()
		if a.String() != tP+"\n" || b.String() != "fine\n" {
			t.Fatalf("a=%d b=%d", a.Len(), b.Len())
		}
	})

	t.Run("withhold", func(t *testing.T) {
		var buf bytes.Buffer
		s := newOutputScreen(nil)
		s.withhold = "more than 4096 keys"
		w := s.stream(&buf)
		n, err := w.Write([]byte("anything\n"))
		w.finish()
		if n != 9 || err != nil || buf.Len() != 0 {
			t.Fatalf("n=%d err=%v out=%d", n, err, buf.Len())
		}
	})

	t.Run("race", func(t *testing.T) {
		s := newOutputScreen([][]byte{[]byte("value-0001-abcdef")})
		var a, b bytes.Buffer
		wa, wb := s.stream(&a), s.stream(&b)
		var wg sync.WaitGroup
		for _, w := range []*screenStream{wa, wb} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					_, _ = w.Write([]byte("x value-0001-abcdef\n"))
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.release()
		}()
		_ = s.notes()
		wg.Wait()
		wa.finish()
		wb.finish()
		for label, buf := range map[string]*bytes.Buffer{"a": &a, "b": &b} {
			out := buf.String()
			if strings.Contains(out, "value-0001-abcdef") {
				t.Fatalf("stream %s leaked a key", label)
			}
			if out != strings.Repeat("x "+tM+"\n", strings.Count(out, "\n")) {
				t.Fatalf("stream %s holds something other than screened records: %d bytes", label, len(out))
			}
		}
	})
}

func TestScreenNotes(t *testing.T) {
	t.Run("nothing", func(t *testing.T) {
		if n := newOutputScreen(nil).notes(); n != nil {
			t.Fatalf("got %d notes", len(n))
		}
	})
	t.Run("two matches", func(t *testing.T) {
		s := newOutputScreen([][]byte{[]byte("value-0001")})
		var a, b bytes.Buffer
		wa, wb := s.stream(&a), s.stream(&b)
		_, _ = wa.Write([]byte("x value-0001\n"))
		_, _ = wb.Write([]byte(".env:1:x\n"))
		wa.finish()
		wb.finish()
		want := "credential screen: 2 match(es) withheld from output (credential_path)"
		if n := s.notes(); len(n) != 1 || n[0] != want {
			t.Fatalf("got %d notes", len(n))
		}
		s.release()
		if s.keys != nil {
			t.Fatal("keys kept after release")
		}
		s.release()
		if n := s.notes(); len(n) != 1 || n[0] != want {
			t.Fatalf("after release: %d notes", len(n))
		}
	})
	t.Run("changed only", func(t *testing.T) {
		s := newOutputScreen(nil)
		s.changed = 2
		want := "credential screen: 2 entr(ies) changed during the read and were skipped; output screened without them"
		if n := s.notes(); len(n) != 1 || n[0] != want {
			t.Fatalf("got %d notes", len(n))
		}
	})
	t.Run("changed and match", func(t *testing.T) {
		s := newOutputScreen(nil)
		s.changed = 1
		var a bytes.Buffer
		w := s.stream(&a)
		_, _ = w.Write([]byte(".env:1:x\n"))
		w.finish()
		n := s.notes()
		if len(n) != 2 ||
			n[0] != "credential screen: 1 entr(ies) changed during the read and were skipped; output screened without them" ||
			n[1] != "credential screen: 1 match(es) withheld from output (credential_path)" {
			t.Fatalf("got %d notes", len(n))
		}
	})
	t.Run("withhold", func(t *testing.T) {
		s := newOutputScreen(nil)
		s.withhold = "more than 4096 keys"
		s.changed = 3
		var a bytes.Buffer
		w := s.stream(&a)
		_, _ = w.Write([]byte(".env:1:x\n"))
		w.finish()
		n := s.notes()
		if len(n) != 1 || n[0] != "credential screen: output withheld; credential files could not be read completely (more than 4096 keys)" {
			t.Fatalf("got %d notes", len(n))
		}
	})
}
