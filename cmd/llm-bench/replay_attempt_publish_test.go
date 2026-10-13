//go:build !windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// publishTestKey is long enough to be redacted (minRedactableKeyLen).
const publishTestKey = "lab_0123456789abcdef"

func claimTemp(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "attempt")
	if err := newAttemptPublisher(dir).claim(); err != nil {
		t.Fatalf("claim: %v", err)
	}
	return dir
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// publishRows: row 1 got a response echoing the key; row 2 replays that
// echo in its request (history) and got no response.
func publishRows() []*recordedRequest {
	now := time.Now()
	return []*recordedRequest{
		{Ordinal: 1, RequestBody: []byte(`{"n":1}`), HeadersAt: now, HTTPStatus: 200, ResponseBody: []byte(`{"echo":"` + publishTestKey + `"}`), ResponseComplete: true},
		{Ordinal: 2, RequestBody: []byte(`{"n":2,"history":"` + publishTestKey + `"}`), TransportErr: errors.New("dial")},
	}
}

func TestAttemptPublisherClaim(t *testing.T) {
	t.Run("fresh dir is created 0700", func(t *testing.T) {
		dir := claimTemp(t)
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("stat %v mode %v; want 0700", err, info.Mode().Perm())
		}
	})
	t.Run("existing dir is refused and untouched", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "attempt")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, "marker")
		if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := newAttemptPublisher(dir).claim(); err == nil {
			t.Fatal("claim of an existing dir succeeded")
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 || entries[0].Name() != "marker" {
			t.Fatalf("existing dir was modified: %v", entries)
		}
	})
	t.Run("missing parent is refused and creates nothing", func(t *testing.T) {
		root := t.TempDir()
		if err := newAttemptPublisher(filepath.Join(root, "absent", "attempt")).claim(); err == nil {
			t.Fatal("claim under a missing parent succeeded")
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Fatalf("claim created %v", entries)
		}
	})
	t.Run("empty path is refused", func(t *testing.T) {
		if err := newAttemptPublisher("").claim(); err == nil {
			t.Fatal("claim of an empty path succeeded")
		}
	})
}

func TestAttemptPublisherWriteRaw(t *testing.T) {
	dir := claimTemp(t)
	refs, aerr := newAttemptPublisher(dir).writeRaw(publishRows(), publishTestKey)
	if aerr != nil {
		t.Fatalf("artifact error: %+v", aerr)
	}
	req := refs[0].Request
	if req == nil || req.Path != "raw/0001.request.json" || req.SHA256 != sha256Hex([]byte(`{"n":1}`)) || req.Bytes != 7 || req.Redacted {
		t.Fatalf("request ref %+v", req)
	}
	resp := refs[0].Response
	stored := []byte(`{"echo":"[REDACTED]"}`)
	if resp == nil || resp.Path != "raw/0001.response.body" || !resp.Redacted || !resp.Complete || resp.SHA256 != sha256Hex(stored) {
		t.Fatalf("response ref %+v", resp)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "raw", "0001.response.body")); err != nil || string(got) != string(stored) {
		t.Fatalf("stored response %q (%v); want the key redacted", got, err)
	}
	echoed := []byte(`{"n":2,"history":"[REDACTED]"}`)
	if r := refs[1].Request; r == nil || !r.Redacted || r.SHA256 != sha256Hex(echoed) {
		t.Fatalf("request 2 ref %+v; an echoed key reaches the next request body and must be redacted there too", r)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "raw", "0002.request.json")); string(got) != string(echoed) {
		t.Fatalf("stored request 2 %q", got)
	}
	if info, _ := os.Stat(filepath.Join(dir, "raw", "0001.request.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("raw file mode %v; want 0600", info.Mode().Perm())
	}
	if refs[1].Response != nil {
		t.Fatalf("transport-error row has a response ref %+v", refs[1].Response)
	}
	if _, err := os.Stat(filepath.Join(dir, "raw", "0002.response.body")); !os.IsNotExist(err) {
		t.Fatalf("a response file exists for a request that got no response: %v", err)
	}
}

func TestAttemptPublisherShortKeyIsNotRedacted(t *testing.T) {
	dir := claimTemp(t)
	rows := []*recordedRequest{{Ordinal: 1, RequestBody: []byte(`{"n":10}`), HeadersAt: time.Now(), HTTPStatus: 200, ResponseBody: []byte(`{"tokens":100}`), ResponseComplete: true}}
	refs, aerr := newAttemptPublisher(dir).writeRaw(rows, "0")
	if aerr != nil || refs[0].Request.Redacted || refs[0].Response.Redacted {
		t.Fatalf("refs %+v %+v err %+v; a key below minRedactableKeyLen must leave evidence untouched", refs[0].Request, refs[0].Response, aerr)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "raw", "0001.response.body")); string(got) != `{"tokens":100}` {
		t.Fatalf("stored response %q was rewritten", got)
	}
}

func TestAttemptPublisherWriteRawFailureIsExplicit(t *testing.T) {
	p := newAttemptPublisher(claimTemp(t))
	write := p.writeFile
	p.writeFile = func(path string, data []byte) error {
		if strings.HasSuffix(path, "0001.response.body") {
			return errors.New("disk full")
		}
		return write(path, data)
	}
	refs, aerr := p.writeRaw(publishRows(), "")
	if aerr == nil || aerr.Code != attemptCodeArtifactWrite || aerr.Stage != attemptStagePublish || !strings.Contains(aerr.Message, "raw/0001.response.body") {
		t.Fatalf("artifact error %+v", aerr)
	}
	if refs[0].Response != nil || refs[0].Request == nil || refs[1].Request == nil {
		t.Fatalf("refs %+v %+v; only the failed file may be null", refs[0], refs[1])
	}
}

func TestAttemptPublisherRawDirSyncFailureNullsEveryRef(t *testing.T) {
	p := newAttemptPublisher(claimTemp(t))
	syncDir := p.syncDir
	p.syncDir = func(dir string) error {
		if filepath.Base(dir) == "raw" {
			return errors.New("fsync failed")
		}
		return syncDir(dir)
	}
	refs, aerr := p.writeRaw(publishRows(), "")
	if aerr == nil || !strings.Contains(aerr.Message, "raw/") {
		t.Fatalf("artifact error %+v", aerr)
	}
	for i, r := range refs {
		if r.Request != nil || r.Response != nil {
			t.Fatalf("ref %d %+v survived an unsynced raw dir", i, r)
		}
	}
}

// Every evidence file, the terminal temp, raw/, the attempt dir and the
// claim's parent are fsynced; observed through the sync seams, not through
// writeFile, so dropping an fsync cannot hide behind a successful write.
func TestAttemptPublisherSyncsEveryFileAndTheClaimParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attempt")
	p := newAttemptPublisher(dir)
	var dirs, files []string
	syncDir, syncFile := p.syncDir, p.syncFile
	p.syncDir = func(d string) error { dirs = append(dirs, d); return syncDir(d) }
	p.syncFile = func(f *os.File) error { files = append(files, filepath.Base(f.Name())); return syncFile(f) }
	if err := p.claim(); err != nil {
		t.Fatal(err)
	}
	if _, aerr := p.writeRaw(publishRows(), publishTestKey); aerr != nil {
		t.Fatalf("artifact error %+v", aerr)
	}
	if err := p.publishTerminal([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"0001.request.json", "0001.response.body", "0002.request.json", "." + attemptTerminalName + ".tmp"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("fsynced files %v, want %v", files, want)
	}
	if want := []string{filepath.Dir(dir), filepath.Join(dir, "raw"), dir}; !reflect.DeepEqual(dirs, want) {
		t.Fatalf("fsynced dirs %v, want %v", dirs, want)
	}
}

func TestAttemptPublisherNoRowsWritesNoRawDir(t *testing.T) {
	dir := claimTemp(t)
	if refs, aerr := newAttemptPublisher(dir).writeRaw(nil, ""); len(refs) != 0 || aerr != nil {
		t.Fatalf("refs %v err %+v", refs, aerr)
	}
	if _, err := os.Stat(filepath.Join(dir, "raw")); !os.IsNotExist(err) {
		t.Fatalf("raw dir exists for an attempt with no requests: %v", err)
	}
}

func TestAttemptPublisherPublishTerminal(t *testing.T) {
	dir := claimTemp(t)
	if err := newAttemptPublisher(dir).publishTerminal([]byte("{}\n")); err != nil {
		t.Fatalf("publishTerminal: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, attemptTerminalName))
	if err != nil || string(got) != "{}\n" {
		t.Fatalf("terminal %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "."+attemptTerminalName+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp terminal left behind: %v", err)
	}
}

func TestAttemptPublisherTerminalRenameFailure(t *testing.T) {
	dir := claimTemp(t)
	p := newAttemptPublisher(dir)
	p.rename = func(string, string) error { return errors.New("rename failed") }
	if err := p.publishTerminal([]byte("{}\n")); err == nil {
		t.Fatal("publishTerminal succeeded with a failing rename")
	}
	for _, name := range []string{attemptTerminalName, "." + attemptTerminalName + ".tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s exists after a failed publication: %v", name, err)
		}
	}
}

func TestAttemptPublisherDirSyncFailureAfterRename(t *testing.T) {
	dir := claimTemp(t)
	p := newAttemptPublisher(dir)
	p.syncDir = func(d string) error {
		if d == dir {
			return errors.New("fsync failed")
		}
		return syncDirectory(d)
	}
	if err := p.publishTerminal([]byte("{}\n")); err == nil {
		t.Fatal("publishTerminal reported success without a durable directory entry")
	}
	if _, err := os.Stat(filepath.Join(dir, attemptTerminalName)); err != nil {
		t.Fatalf("attempt.json should be visible after the rename: %v", err)
	}
}

func TestWriteSyncedRefusesExisting(t *testing.T) {
	p := newAttemptPublisher(t.TempDir())
	path := filepath.Join(p.dir, "f")
	if err := p.writeSynced(path, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := p.writeSynced(path, []byte("b")); err == nil {
		t.Fatal("second write replaced an existing evidence file")
	}
	if got, _ := os.ReadFile(path); string(got) != "a" {
		t.Fatalf("file %q; evidence must be immutable", got)
	}
}
