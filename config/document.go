package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// OriginSource identifies where a Document's bytes came from.
type OriginSource string

const (
	// OriginEnvOverride means the path came from $GO_LLM_CONFIG.
	OriginEnvOverride OriginSource = "env-override"
	// OriginWorkingDir means ./models.json in the working directory won.
	OriginWorkingDir OriginSource = "working-dir"
	// OriginUserConfig means UserConfigDir/go-llm/models.json won.
	OriginUserConfig OriginSource = "user-config"
	// OriginLegacy means the legacy ~/.config/go-llm/models.json path won.
	OriginLegacy OriginSource = "legacy"
	// OriginExplicit means the caller supplied the path directly.
	OriginExplicit OriginSource = "explicit-path"
	// OriginProfile means the bytes came from a named profile store entry.
	OriginProfile OriginSource = "profile"
	// OriginProgrammatic means the config was built in memory, not loaded.
	OriginProgrammatic OriginSource = "programmatic"
)

// Origin describes a Document's provenance. Path is host-side detail and must
// never cross a projection boundary (panels see Source only).
type Origin struct {
	Source OriginSource
	Path   string
}

// Document owns a configuration's bytes, provenance, and revision. authored
// is the config exactly as written (pre-expansion, pre-defaults, NOT
// validated); effective is what Load produces today. All state is guarded by
// mu — Documents are safe for concurrent use.
type Document struct {
	mu             sync.Mutex
	rawBytes       []byte
	revision       string
	origin         Origin
	originIdentity string
	authored       *Config
	effective      *Config
	providerDrops  map[string]struct{} // provider names removed since the rawBytes baseline
	env            func(string) (string, bool)
	readOnly       *Diagnostic
}

// DocumentOptions configures document construction (spec §7).
type DocumentOptions struct {
	// LookupEnv resolves ${ENV} presence checks and expansion for this
	// document. nil = ambient os.LookupEnv, consulted on parse and every
	// mutation (compatibility). Hosts needing deterministic
	// document-lifetime behavior pass a stable snapshot-backed function.
	// A ("", true) result is treated as unset by api_key expansion (same
	// as ("", false)). The lookup runs under the document's internal
	// mutex during mutations — keep it fast; a blocking lookup stalls all
	// Document readers. The lookup must not call back into the same
	// Document (its mutex is held during mutations and is not reentrant).
	LookupEnv func(string) (string, bool)
}

// ParseDocument is the low-level parse seam: caller-owned bytes in (copied,
// never aliased), Document out. Performs no filesystem I/O. The bytes go
// through the same finalize pipeline as Load — validation is NOT deferred.
func ParseDocument(data []byte, origin Origin, opts DocumentOptions) (*Document, error) {
	owned := append([]byte(nil), data...)
	return newDocumentEnv(owned, origin, opts.LookupEnv)
}

// LoadDocument reads a models.json from an explicit path into a Document.
// The effective view goes through the same finalize pipeline as Load; the
// authored view keeps the file's content exactly as written (api_key
// literals unexpanded, defaults unmaterialized).
func LoadDocument(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, diagWrap(CodeIO, SubjectNone, "", fmt.Errorf("config: read %q: %w", path, err))
	}
	return newDocument(data, Origin{Source: OriginExplicit, Path: path})
}

// DefaultDocument resolves the same discovery order as Default and loads the
// winning path as a Document, retaining WHICH rule won as the origin.
func DefaultDocument() (*Document, error) {
	path, src, err := discoverConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, diagWrap(CodeIO, SubjectNone, "", fmt.Errorf("config: read %q: %w", path, err))
	}
	return newDocument(data, Origin{Source: src, Path: path})
}

// NewDocumentFromBytes builds a Document from raw models.json bytes with the
// caller's origin (profile stores, embedded catalogs, tests). The input
// slice is COPIED by ParseDocument — the document never aliases caller
// memory.
func NewDocumentFromBytes(data []byte, origin Origin) (*Document, error) {
	return ParseDocument(data, origin, DocumentOptions{})
}

// newDocument is the ambient-env construction path shared by LoadDocument
// and DefaultDocument; data must already be owned by the callee.
func newDocument(data []byte, origin Origin) (*Document, error) {
	return newDocumentEnv(data, origin, nil)
}

func newDocumentEnv(data []byte, origin Origin, env func(string) (string, bool)) (*Document, error) {
	var authored Config
	if err := json.Unmarshal(data, &authored); err != nil {
		return nil, diagWrap(CodeParseError, SubjectNone, "", fmt.Errorf("config: parse %q: %w", origin.Path, err))
	}
	var effective Config
	if err := json.Unmarshal(data, &effective); err != nil {
		return nil, diagWrap(CodeParseError, SubjectNone, "", fmt.Errorf("config: parse %q: %w", origin.Path, err))
	}
	if err := effective.finalizeEnv(env); err != nil {
		return nil, err
	}
	diag, found, err := detectCollisions(data)
	if err != nil {
		return nil, diagWrap(CodeParseError, SubjectNone, "",
			fmt.Errorf("config: parse %q: %w", origin.Path, err))
	}
	var readOnly *Diagnostic
	if found {
		readOnly = &diag
	}
	sum := sha256.Sum256(data)
	return &Document{
		rawBytes:       data,
		revision:       hex.EncodeToString(sum[:]),
		origin:         origin,
		originIdentity: documentPathIdentity(origin.Path),
		authored:       &authored,
		effective:      &effective,
		env:            env,
		readOnly:       readOnly,
	}, nil
}

func documentPathIdentity(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return filepath.Clean(abs)
}

// Origin returns the document's provenance.
func (d *Document) Origin() Origin {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.origin
}

// Revision returns the sha256 hex digest of the document's loaded bytes.
func (d *Document) Revision() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.revision
}

// Config returns a deep clone of the effective (expanded, defaulted)
// configuration — callers can never mutate the document through it.
func (d *Document) Config() *Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.effective.clone()
}

// clone deep-copies via JSON round-trip. Configs are no longer solely
// parse-derived: programmatic mutations must uphold the parse-boundary
// invariants (validate plus the entry-point guards enforce them; e.g.
// Duration's non-negative rule). The clone panics remain the backstop for
// bugs, never a reachable rejection path. Expansion is NOT re-triggered —
// it lives in finalize, not UnmarshalJSON.
func (c *Config) clone() *Config {
	data, err := json.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("config: clone marshal: %v", err))
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("config: clone unmarshal: %v", err))
	}
	return &out
}
