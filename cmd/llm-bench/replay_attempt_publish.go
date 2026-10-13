package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// attemptTerminalName is the terminal record. It is published last, so its
// presence means every raw file it references is written and synced.
const attemptTerminalName = "attempt.json"

// attemptRedacted replaces the candidate key in stored evidence.
const attemptRedacted = "[REDACTED]"

// minRedactableKeyLen is the shortest key redaction rewrites. A shorter value
// cannot be told apart from ordinary data, and replacing it would corrupt the
// evidence (a key "0" would rewrite every zero). Lab attempt keys are 68
// bytes (spec amendment A8).
const minRedactableKeyLen = 16

func redactableKey(key string) bool { return len(key) >= minRedactableKeyLen }

// attemptFileRef points at one raw evidence file inside the attempt dir.
// SHA256 hashes the stored bytes; Redacted is true when the candidate key was
// replaced in them.
type attemptFileRef struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Bytes    int    `json:"bytes"`
	Redacted bool   `json:"redacted"`
}

// attemptResponseRef is a response file: Complete is false when the body
// read failed or hit the cap.
type attemptResponseRef struct {
	attemptFileRef
	Complete bool `json:"complete"`
}

// attemptRawRefs are one row's evidence files; nil means not written.
type attemptRawRefs struct {
	Request  *attemptFileRef
	Response *attemptResponseRef
}

// attemptPublisher writes one attempt directory. The function fields are the
// production file operations; tests replace them to observe fsyncs or inject
// failures.
type attemptPublisher struct {
	dir       string
	writeFile func(path string, data []byte) error
	syncFile  func(f *os.File) error
	syncDir   func(dir string) error
	rename    func(oldpath, newpath string) error
}

func newAttemptPublisher(dir string) *attemptPublisher {
	p := &attemptPublisher{dir: dir, syncFile: (*os.File).Sync, syncDir: syncDirectory, rename: os.Rename}
	p.writeFile = p.writeSynced
	return p
}

// claim creates the attempt directory, which must not exist: the Mkdir elects
// this process as its sole writer, so later renames inside it need no
// no-replace step. The parent is synced so the claim itself is durable.
func (p *attemptPublisher) claim() error {
	if strings.TrimSpace(p.dir) == "" {
		return errors.New("-attempt-dir is required")
	}
	if err := os.Mkdir(p.dir, 0o700); err != nil {
		return fmt.Errorf("claim attempt dir: %w", err)
	}
	return p.syncDir(filepath.Dir(filepath.Clean(p.dir)))
}

// writeRaw writes every row's request and response bytes under raw/, then
// syncs raw/. Both directions are redacted: a key the server echoes in reply
// N re-enters the conversation and reaches request N+1. A ref is set only for
// a file written and synced; every failure is named in the returned
// artifact_write error, and writing continues so one bad file costs only
// itself.
func (p *attemptPublisher) writeRaw(rows []*recordedRequest, apiKey string) ([]attemptRawRefs, *attemptError) {
	refs := make([]attemptRawRefs, len(rows))
	if len(rows) == 0 {
		return refs, nil
	}
	rawDir := filepath.Join(p.dir, "raw")
	if err := os.Mkdir(rawDir, 0o700); err != nil {
		return refs, &attemptError{Code: attemptCodeArtifactWrite, Stage: attemptStagePublish, Message: "unwritten evidence: raw/"}
	}
	var failed []string
	for i, row := range rows {
		name := fmt.Sprintf("raw/%04d.request.json", row.Ordinal)
		body, redacted := redactAttemptKey(row.RequestBody, apiKey)
		if ref, err := p.writeRef(name, body, redacted); err != nil {
			failed = append(failed, name)
		} else {
			refs[i].Request = ref
		}
		if row.HeadersAt.IsZero() {
			continue // no response arrived, so there is nothing to keep
		}
		name = fmt.Sprintf("raw/%04d.response.body", row.Ordinal)
		body, redacted = redactAttemptKey(row.ResponseBody, apiKey)
		if ref, err := p.writeRef(name, body, redacted); err != nil {
			failed = append(failed, name)
		} else {
			refs[i].Response = &attemptResponseRef{attemptFileRef: *ref, Complete: row.ResponseComplete}
		}
	}
	if err := p.syncDir(rawDir); err != nil {
		// Unsynced entries are not durable evidence: reference none of them.
		refs = make([]attemptRawRefs, len(rows))
		failed = append(failed, "raw/")
	}
	if len(failed) == 0 {
		return refs, nil
	}
	return refs, &attemptError{Code: attemptCodeArtifactWrite, Stage: attemptStagePublish, Message: "unwritten evidence: " + strings.Join(failed, ", ")}
}

func (p *attemptPublisher) writeRef(name string, data []byte, redacted bool) (*attemptFileRef, error) {
	if err := p.writeFile(filepath.Join(p.dir, filepath.FromSlash(name)), data); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &attemptFileRef{Path: name, SHA256: hex.EncodeToString(sum[:]), Bytes: len(data), Redacted: redacted}, nil
}

// publishTerminal writes attempt.json through a synced temp file and a
// same-directory rename (atomic visibility on darwin and linux), then syncs
// the directory. A failed directory sync after the rename leaves attempt.json
// visible but not confirmed durable; the caller exits 1 and the lab treats a
// present, parsing attempt.json as authoritative (D3).
func (p *attemptPublisher) publishTerminal(data []byte) error {
	tmp := filepath.Join(p.dir, "."+attemptTerminalName+".tmp")
	if err := p.writeFile(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write terminal: %w", err)
	}
	if err := p.rename(tmp, filepath.Join(p.dir, attemptTerminalName)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish terminal: %w", err)
	}
	if err := p.syncDir(p.dir); err != nil {
		return fmt.Errorf("sync attempt dir (attempt.json visible, not confirmed durable): %w", err)
	}
	return nil
}

// writeSynced creates path exclusively (evidence is never replaced), writes
// data and fsyncs it. On darwin, Go's File.Sync issues F_FULLFSYNC.
func (p *attemptPublisher) writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := p.syncFile(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// redactAttemptKey replaces exact occurrences of a redactable key in raw
// bytes. Raw evidence has no structure to respect, so this is byte-level;
// attempt.json is redacted field by field instead (redactRecordKey).
func redactAttemptKey(data []byte, key string) ([]byte, bool) {
	if !redactableKey(key) || !bytes.Contains(data, []byte(key)) {
		return data, false
	}
	return bytes.ReplaceAll(data, []byte(key), []byte(attemptRedacted)), true
}

// syncDirectory fsyncs a directory so its entries are durable.
func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
