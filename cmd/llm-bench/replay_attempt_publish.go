package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
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
// a file written and synced, and only a ref makes a file evidence. Every path
// without a ref is named in the returned artifact_write error, though a file
// may exist there (partial, or unsynced). Writing continues after a bad file,
// so it costs only itself. The error's message is llm-bench's own text; the
// underlying causes, OS text included, come back as the last value for the
// caller to put on stderr (spec amendment A4).
func (p *attemptPublisher) writeRaw(rows []*recordedRequest, apiKey string) ([]attemptRawRefs, *attemptError, []error) {
	refs := make([]attemptRawRefs, len(rows))
	if len(rows) == 0 {
		return refs, nil, nil
	}
	rawDir := filepath.Join(p.dir, "raw")
	if err := os.Mkdir(rawDir, 0o700); err != nil {
		return refs, unreferencedEvidence("raw/"), []error{fmt.Errorf("create raw/: %w", err)}
	}
	// raw/'s own entry in the attempt dir must be durable before any file in it
	// is referenced, or attempt.json could outlive the directory holding its
	// evidence. Like an unsynced raw/, a failure references nothing.
	if err := p.syncDir(p.dir); err != nil {
		return refs, unreferencedEvidence("raw/"), []error{fmt.Errorf("sync attempt dir after creating raw/: %w", err)}
	}
	var failed []string
	var causes []error
	for i, row := range rows {
		name := fmt.Sprintf("raw/%04d.request.json", row.Ordinal)
		body, redacted := redactAttemptKey(row.RequestBody, apiKey)
		if ref, err := p.writeRef(name, body, redacted); err != nil {
			failed, causes = append(failed, name), append(causes, err)
		} else {
			refs[i].Request = ref
		}
		if row.HeadersAt.IsZero() {
			continue // no response arrived, so there is nothing to keep
		}
		name = fmt.Sprintf("raw/%04d.response.body", row.Ordinal)
		body, redacted = redactAttemptKey(row.ResponseBody, apiKey)
		if ref, err := p.writeRef(name, body, redacted); err != nil {
			failed, causes = append(failed, name), append(causes, err)
		} else {
			refs[i].Response = &attemptResponseRef{attemptFileRef: *ref, Complete: row.ResponseComplete}
		}
	}
	if err := p.syncDir(rawDir); err != nil {
		// Unsynced entries are not durable evidence: reference none of them.
		refs = make([]attemptRawRefs, len(rows))
		failed, causes = append(failed, "raw/"), append(causes, fmt.Errorf("sync raw/: %w", err))
	}
	if len(failed) == 0 {
		return refs, nil, nil
	}
	return refs, unreferencedEvidence(failed...), causes
}

// unreferencedEvidence is the artifact_write error naming paths that no ref
// in requests[] points at. The paths are not evidence even when a file exists
// there: it may be partial, or fully written but not durable.
func unreferencedEvidence(paths ...string) *attemptError {
	return &attemptError{Code: attemptCodeArtifactWrite, Stage: attemptStagePublish, Message: "unreferenced evidence: " + strings.Join(paths, ", ")}
}

func (p *attemptPublisher) writeRef(name string, data []byte, redacted bool) (*attemptFileRef, error) {
	if err := p.writeFile(filepath.Join(p.dir, filepath.FromSlash(name)), data); err != nil {
		return nil, fmt.Errorf("write %s: %w", name, err)
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

// redactAttemptKey replaces credential spans in JSON strings without changing
// any surrounding evidence, including duplicate members and escape spellings.
// The string scanner also handles malformed bodies and unterminated strings.
// Literal occurrences elsewhere retain the original byte-level redaction.
func redactAttemptKey(data []byte, key string) ([]byte, bool) {
	if !redactableKey(key) {
		return data, false
	}
	literal := func(src []byte) ([]byte, bool) {
		if !bytes.Contains(src, []byte(key)) {
			return src, false
		}
		return bytes.ReplaceAll(src, []byte(key), []byte(attemptRedacted)), true
	}
	// KMP matches decoded bytes in linear time. The ring holds source offsets
	// for only the last key-length bytes, not an offset for every body byte.
	prefix, starts := make([]int, len(key)), make([]int, len(key))
	for i, matched := 1, 0; i < len(key); i++ {
		for matched > 0 && key[i] != key[matched] {
			matched = prefix[matched-1]
		}
		if key[i] == key[matched] {
			matched++
		}
		prefix[i] = matched
	}
	var out []byte
	var decoded [utf8.UTFMax]byte
	keyIsUTF8 := utf8.ValidString(key)
	quoted, matched, seen, copied := false, 0, 0, 0
	for i := 0; i < len(data); {
		if data[i] == '"' {
			quoted, matched, seen = !quoted, 0, 0
			i++
			continue
		}
		if !quoted {
			i++
			continue
		}
		r, width := attemptJSONRune(data[i:])
		chars := utf8.AppendRune(decoded[:0], r)
		if !keyIsUTF8 && r == utf8.RuneError && width == 1 {
			// JSON cannot escape invalid UTF-8 credential bytes, but a body
			// may contain them literally (and json.Valid accepts them).
			chars = data[i : i+1]
		}
		for j, b := range chars {
			starts[seen%len(key)] = i
			if j != 0 {
				starts[seen%len(key)] = -1 // a match must start at a rune boundary
			}
			seen++
			for matched > 0 && b != key[matched] {
				matched = prefix[matched-1]
			}
			if b == key[matched] {
				matched++
			}
			if matched != len(key) {
				continue
			}
			start := starts[(seen-len(key))%len(key)]
			if start < 0 || j != len(chars)-1 {
				matched = prefix[matched-1]
				continue
			}
			if out == nil {
				out = make([]byte, 0, len(data))
			}
			// Only inspect original bytes: a replacement adjacent to original
			// text might itself resemble the key and must not be redacted again.
			segment, _ := literal(data[copied:start])
			out = append(out, segment...)
			out = append(out, attemptRedacted...)
			copied, matched = i+width, 0
		}
		i += width
	}
	if out == nil {
		return literal(data)
	}
	segment, _ := literal(data[copied:])
	out = append(out, segment...)
	return out, true
}

// attemptJSONRune returns one decoded JSON character and its original width.
// Invalid or incomplete escapes are consumed literally so a malformed response
// still retains its evidence while complete credential spans can be removed.
func attemptJSONRune(data []byte) (rune, int) {
	if data[0] != '\\' || len(data) < 2 {
		return utf8.DecodeRune(data)
	}
	switch data[1] {
	case '"', '\\', '/':
		return rune(data[1]), 2
	case 'b':
		return '\b', 2
	case 'f':
		return '\f', 2
	case 'n':
		return '\n', 2
	case 'r':
		return '\r', 2
	case 't':
		return '\t', 2
	case 'u':
		if len(data) >= 6 {
			if v, err := strconv.ParseUint(string(data[2:6]), 16, 16); err == nil {
				r := rune(v)
				if !utf16.IsSurrogate(r) {
					return r, 6
				}
				if len(data) >= 12 && data[6] == '\\' && data[7] == 'u' {
					if low, err := strconv.ParseUint(string(data[8:12]), 16, 16); err == nil {
						if pair := utf16.DecodeRune(r, rune(low)); pair != utf8.RuneError {
							return pair, 12
						}
					}
				}
				return utf8.RuneError, 6
			}
		}
	}
	return '\\', 1
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
