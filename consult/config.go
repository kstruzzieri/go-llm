// Package consult runs one external subscription CLI for a single-shot
// advisory judgment (#382). It owns the bounded host runner, the Claude
// adapter and the unsigned receipt. It is not a provider, a Router member
// or a tool; consultant commands come only from local config.
package consult

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// ErrDisabled reports that no consultants file exists at the default path;
// /consult is then unavailable rather than misconfigured.
var ErrDisabled = errors.New("consult: no consultants configured")

// Consultant is one explicitly trusted local consultant declaration.
type Consultant struct {
	Name    string `json:"name"`
	Adapter string `json:"adapter"` // only "claude" in v1
	// Command is the absolute path of a regular file (never a symlink) that
	// is executed as-is; the adapter owns argv.
	Command string `json:"command"`
	// SHA256 is an optional lowercase hex digest the runner re-verifies
	// immediately before exec.
	SHA256               string `json:"sha256,omitempty"`
	Model                string `json:"model"`
	TimeoutSeconds       int    `json:"timeout_seconds,omitempty"`  // 0 => 120, max 300
	MaxOutputBytes       int    `json:"max_output_bytes,omitempty"` // 0 => 1 MiB, max 1 MiB
	TrustedProcessEgress bool   `json:"trusted_process_egress"`     // must be true: vendor traffic bypasses #477
}

// Config is the on-disk shape of consultants.json.
type Config struct {
	Version     int          `json:"version"`
	Consultants []Consultant `json:"consultants"`
}

const (
	defaultTimeoutSeconds = 120
	maxTimeoutSeconds     = 300
	maxOutputBytes        = 1 << 20
	// maxConfigBytes bounds the consultants file itself. It names the
	// executable to run, so it is read under the same discipline as that
	// executable: bounded, regular, and the same file when opened as when
	// inspected.
	maxConfigBytes = 65536
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DefaultPath is go-llm/consultants.json under os.UserConfigDir
// ($XDG_CONFIG_HOME on Linux, ~/Library/Application Support on macOS).
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("consult: user config dir: %w", err)
	}
	return filepath.Join(dir, "go-llm", "consultants.json"), nil
}

// Load reads and validates the consultants file. An explicit path must be
// absolute and must load. An empty path uses DefaultPath, and /consult is
// disabled rather than misconfigured whenever that default is unusable: both
// a missing file and an unresolvable config directory (no HOME, as in a
// stripped test or service environment) yield an error satisfying
// errors.Is(err, ErrDisabled).
func Load(explicit string) (map[string]Consultant, error) {
	path := explicit
	if path == "" {
		var err error
		if path, err = DefaultPath(); err != nil {
			// No resolvable config directory is indistinguishable, for the
			// caller, from no consultants file: neither is a misconfiguration
			// and both simply mean /consult is unavailable.
			return nil, fmt.Errorf("%w: %v", ErrDisabled, err)
		}
	} else if !filepath.IsAbs(path) {
		return nil, errors.New("consult: config path must be absolute")
	}
	raw, err := readConfigFile(path)
	if err != nil {
		if explicit == "" && errors.Is(err, os.ErrNotExist) {
			return nil, ErrDisabled
		}
		return nil, fmt.Errorf("consult: read config: %w", err)
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("consult: parse config: %w", err)
	}
	// Reject trailing content after the object: one file, one config.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("consult: parse config: trailing content after JSON object")
	}
	if cfg.Version != 1 {
		return nil, fmt.Errorf("consult: unsupported config version %d", cfg.Version)
	}
	out := make(map[string]Consultant, len(cfg.Consultants))
	for i := range cfg.Consultants {
		c := cfg.Consultants[i]
		if err := validate(&c); err != nil {
			return nil, fmt.Errorf("consult: consultant %d: %w", i, err)
		}
		if _, dup := out[c.Name]; dup {
			return nil, fmt.Errorf("consult: duplicate consultant name %q", c.Name)
		}
		out[c.Name] = c
	}
	return out, nil
}

// afterConfigLstat runs between readConfigFile's Lstat and its open. It is nil
// in production and exists only so a test can swap the path in that window; it
// is package-level state, so no test that sets it may t.Parallel.
var afterConfigLstat func(path string)

// readConfigFile reads the consultants file under the same discipline Load
// applies to the command it declares. The path must be a regular file and not
// a symlink, the opened descriptor must still be that same file, and the
// content is bounded, so neither a redirected path, a file swapped during the
// open, a FIFO nor an unbounded stream can steer or stall startup. The
// identity check is what closes the Lstat-to-open window: without it a
// validated file and an obeyed file can be two different inodes.
func readConfigFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if afterConfigLstat != nil {
		afterConfigLstat(path)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("config path must not be a symlink")
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("config path must be a regular file")
	}
	f, err := openConfigFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() {
		return nil, errors.New("config path must be a regular file")
	}
	if !os.SameFile(before, after) {
		return nil, errors.New("config file identity changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxConfigBytes {
		return nil, fmt.Errorf("config is too large: over %d bytes", maxConfigBytes)
	}
	return raw, nil
}

// validate checks one consultant declaration and applies the documented
// defaults for timeout_seconds and max_output_bytes in place.
func validate(c *Consultant) error {
	switch {
	case !nameRE.MatchString(c.Name):
		return errors.New("name must match ^[a-z0-9][a-z0-9-]{0,31}$")
	case c.Adapter != claudeAdapter:
		return fmt.Errorf("unsupported adapter %q", c.Adapter)
	case !filepath.IsAbs(c.Command):
		return errors.New("command must be an absolute path")
	case c.SHA256 != "" && !shaRE.MatchString(c.SHA256):
		return errors.New("sha256 must be 64 lowercase hex characters")
	case !claudeModels[c.Model]:
		return fmt.Errorf("unsupported model %q for adapter claude", c.Model)
	case c.TimeoutSeconds < 0 || c.TimeoutSeconds > maxTimeoutSeconds:
		return fmt.Errorf("timeout_seconds must be 0..%d", maxTimeoutSeconds)
	case c.MaxOutputBytes < 0 || c.MaxOutputBytes > maxOutputBytes:
		return fmt.Errorf("max_output_bytes must be 0..%d", maxOutputBytes)
	case !c.TrustedProcessEgress:
		return errors.New("trusted_process_egress must be true: consultant traffic is not filtered by go-llm")
	}
	if _, err := validateCommand(c.Command); err != nil {
		return err
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = defaultTimeoutSeconds
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = maxOutputBytes
	}
	return nil
}

// validateCommand is shared by config loading and every executable launch.
func validateCommand(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("command must be an absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("command must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("command must be a regular file")
	}
	// Anyone who can rewrite the file can replace the consultant, and a digest
	// recorded once does not survive that; the pre-exec re-verification would
	// only turn it into a run-time failure.
	if info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("command must not be group- or world-writable")
	}
	// Lstat only inspects the leaf: reject a command reached through a
	// symlinked parent directory too, so the path cannot be redirected.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	if resolved != path {
		return nil, errors.New("command path must not traverse symlinks")
	}
	return info, nil
}
