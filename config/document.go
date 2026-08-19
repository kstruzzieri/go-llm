package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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
	mu        sync.Mutex
	rawBytes  []byte
	revision  string
	origin    Origin
	authored  *Config
	effective *Config
}

// LoadDocument reads a models.json from an explicit path into a Document.
// The effective view goes through the same finalize pipeline as Load; the
// authored view keeps the file's content exactly as written (api_key
// literals unexpanded, defaults unmaterialized).
func LoadDocument(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	return newDocument(data, Origin{Source: OriginExplicit, Path: path})
}

func newDocument(data []byte, origin Origin) (*Document, error) {
	var authored Config
	if err := json.Unmarshal(data, &authored); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", origin.Path, err)
	}
	var effective Config
	if err := json.Unmarshal(data, &effective); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", origin.Path, err)
	}
	if err := effective.finalize(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &Document{
		rawBytes:  data,
		revision:  hex.EncodeToString(sum[:]),
		origin:    origin,
		authored:  &authored,
		effective: &effective,
	}, nil
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

// clone deep-copies via JSON round-trip: every Config field is
// JSON-serializable by construction (that is how configs load). Expansion is
// NOT re-triggered — it lives in finalize, not UnmarshalJSON.
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
