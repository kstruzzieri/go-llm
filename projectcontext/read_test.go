package projectcontext

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReadCappedReadsRegularFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(p, []byte("hello context"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, hash, size, truncated, found, err := readCapped(context.Background(), p, defaultMaxBytes)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if !found {
		t.Fatal("readCapped: want found=true")
	}
	if truncated {
		t.Fatal("readCapped: want truncated=false")
	}
	if content != "hello context" {
		t.Fatalf("readCapped: content=%q", content)
	}
	if hash == ([32]byte{}) {
		t.Error("readCapped: hash is zero, want content hash")
	}
	if size != 13 {
		t.Errorf("readCapped: size = %d, want 13", size)
	}
}

func TestReadCappedMissingFileNotFound(t *testing.T) {
	_, _, _, _, found, err := readCapped(context.Background(), filepath.Join(t.TempDir(), "nope.md"), defaultMaxBytes)
	if err != nil {
		t.Fatalf("readCapped: want nil error for missing file, got %v", err)
	}
	if found {
		t.Fatal("readCapped: want found=false for missing file")
	}
}

func TestReadCappedSkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.md")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "AGENTS.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, _, _, _, found, err := readCapped(context.Background(), link, defaultMaxBytes)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if found {
		t.Fatal("readCapped: must not follow a symlink (want found=false)")
	}
}

func TestReadStreamErrorReturnsNoEvidence(t *testing.T) {
	wantErr := errors.New("read failed")
	r := &callbackReader{read: func(_ int, p []byte) (int, error) {
		copy(p, "partial")
		return len("partial"), wantErr
	}}

	content, hash, size, truncated, err := readStream(context.Background(), r, 3)
	if !errors.Is(err, wantErr) {
		t.Fatalf("readStream: error = %v, want %v", err, wantErr)
	}
	if content != "" || hash != ([32]byte{}) || size != 0 || truncated {
		t.Errorf("readStream: failed result = {Content:%q Hash:%s Size:%d Truncated:%t}, want no evidence", content, fmtHash(hash), size, truncated)
	}
}

func TestReadStreamCancellationStopsBeforeUnreadData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var bufferSize int
	r := &callbackReader{read: func(call int, p []byte) (int, error) {
		bufferSize = len(p)
		if call == 1 {
			for i := range p {
				p[i] = 'a'
			}
			cancel()
			return len(p), nil
		}
		copy(p, "unread tail")
		return len("unread tail"), io.EOF
	}}

	content, hash, size, truncated, err := readStream(ctx, r, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readStream: error = %v, want context.Canceled", err)
	}
	if r.calls != 1 {
		t.Errorf("readStream: read calls = %d, want 1", r.calls)
	}
	if bufferSize != 32*1024 {
		t.Errorf("readStream: buffer size = %d, want %d", bufferSize, 32*1024)
	}
	if content != "" || hash != ([32]byte{}) || size != 0 || truncated {
		t.Errorf("readStream: canceled result = {Content:%q Hash:%s Size:%d Truncated:%t}, want no evidence", content, fmtHash(hash), size, truncated)
	}
}

func TestReadStreamCanceledBeforeRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &callbackReader{read: func(_ int, _ []byte) (int, error) {
		return 0, io.EOF
	}}

	content, hash, size, truncated, err := readStream(ctx, r, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readStream: error = %v, want context.Canceled", err)
	}
	if r.calls != 0 {
		t.Errorf("readStream: read calls = %d, want 0", r.calls)
	}
	if content != "" || hash != ([32]byte{}) || size != 0 || truncated {
		t.Errorf("readStream: canceled initial result = {Content:%q Hash:%s Size:%d Truncated:%t}, want no evidence", content, fmtHash(hash), size, truncated)
	}
}

func TestReadStreamCancellationDuringFinalBytesAndEOFReturnsNoEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &callbackReader{read: func(_ int, p []byte) (int, error) {
		copy(p, "abc")
		cancel()
		return 3, io.EOF
	}}

	content, hash, size, truncated, err := readStream(ctx, r, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readStream: error = %v, want context.Canceled", err)
	}
	if r.calls != 1 {
		t.Errorf("readStream: read calls = %d, want 1", r.calls)
	}
	if content != "" || hash != ([32]byte{}) || size != 0 || truncated {
		t.Errorf("readStream: canceled final result = {Content:%q Hash:%s Size:%d Truncated:%t}, want no evidence", content, fmtHash(hash), size, truncated)
	}
}

type callbackReader struct {
	calls int
	read  func(call int, p []byte) (int, error)
}

func (r *callbackReader) Read(p []byte) (int, error) {
	r.calls++
	return r.read(r.calls, p)
}
