package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// verifyConfigName is the ONLY file a workspace may declare a post-write
	// verification command in (#347). It is read from the workspace root and
	// nowhere else: no ancestor search, so an outer repository cannot arm a
	// command for a session rooted in one of its subdirectories.
	verifyConfigName = ".golem.json"
	// verifyConfigMaxBytes bounds the read. The whole schema is a handful of
	// short strings; anything larger is a mistake or an attack, not config.
	verifyConfigMaxBytes = 64 * 1024
	// verifyDefaultTimeout applies when timeout_seconds is absent.
	verifyDefaultTimeout = 60 * time.Second
	// verifyMaxTimeoutSeconds mirrors the exec ceiling. An out-of-range value
	// is refused rather than clamped: a workspace-declared value is explicit,
	// so a mistake should be surfaced, not quietly rewritten.
	verifyMaxTimeoutSeconds = 600
	// verifyMaxArgv and verifyMaxArgvBytes bound what the approval prompt has
	// to render. The file may be 64 KiB, and unlike run_command's argv (which
	// comes from the already-gated model) this one comes from a repository
	// that may be hostile: thousands of arguments would scroll the [y/N/a]
	// question off the screen and turn the prompt into a blind approval. The
	// same bound keeps the command line inside the model-visible observation's
	// budget. A real check needs a handful of short arguments.
	verifyMaxArgv      = 64
	verifyMaxArgvBytes = 4096
	// verifyMessageMaxBytes bounds every diagnostic this loader emits. The
	// messages quote workspace-controlled text — encoding/json echoes an
	// unknown field's name verbatim, and a bad dir is quoted back — so a single
	// enormous key in a 64 KiB file would otherwise flood the startup output.
	verifyMessageMaxBytes = 240
)

// golemWorkspaceConfig is the whole of .golem.json. Every field is optional;
// unknown fields are refused, so a typo disables verification loudly instead
// of silently doing nothing.
type golemWorkspaceConfig struct {
	Verify *verifySpec `json:"verify"`
}

// verifySpec is the workspace-declared verification command. Structured argv
// only: there is no shell, and no field through which the workspace can supply
// an environment, an output limit, or a sandbox policy.
type verifySpec struct {
	Argv           []string `json:"argv"`
	Dir            string   `json:"dir"`
	TimeoutSeconds *int     `json:"timeout_seconds"`
}

// afterVerifyConfigLstatForTest lets the regression test deterministically
// replace the pathname after it has been inspected but before it is opened.
// It is nil in production.
var afterVerifyConfigLstatForTest func()

// Timeout is the effective per-run deadline.
func (s *verifySpec) Timeout() time.Duration {
	if s.TimeoutSeconds == nil {
		return verifyDefaultTimeout
	}
	return time.Duration(*s.TimeoutSeconds) * time.Second
}

// loadVerifyConfig reads and validates the workspace's verification command.
//
// It returns (nil, nil) when the file is absent or declares no verify key —
// the byte-for-byte unchanged path. Every other problem is an error the caller
// surfaces as a startup warning and continues without verification: a
// malformed workspace file must never brick the CLI.
func loadVerifyConfig(root string) (*verifySpec, error) {
	path := filepath.Join(root, verifyConfigName)
	fail := func(format string, args ...any) (*verifySpec, error) {
		return nil, errors.New(truncateMessage(
			verifyConfigName + ": " + fmt.Sprintf(format, args...)))
	}

	// Lstat, not Stat: a symlink here would let a workspace point the loader
	// at a file outside it. The opened descriptor is compared below, so a
	// replacement between this check and open is rejected too.
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return fail("%v", err)
	}
	if !fi.Mode().IsRegular() {
		return fail("must be a regular file, got mode %s", fi.Mode().Type())
	}
	if fi.Size() > verifyConfigMaxBytes {
		return fail("too large (%d bytes, limit %d)", fi.Size(), verifyConfigMaxBytes)
	}
	if afterVerifyConfigLstatForTest != nil {
		afterVerifyConfigLstatForTest()
	}

	f, err := os.Open(path)
	if err != nil {
		return fail("%v", err)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return fail("%v", err)
	}
	if !opened.Mode().IsRegular() {
		return fail("must be a regular file, got mode %s", opened.Mode().Type())
	}
	if !os.SameFile(fi, opened) {
		return fail("changed while opening")
	}

	// The pre-open size is only an early refusal. Limit the descriptor read as
	// well because the file can grow after that check.
	raw, err := io.ReadAll(io.LimitReader(f, verifyConfigMaxBytes+1))
	if err != nil {
		return fail("%v", err)
	}
	if len(raw) > verifyConfigMaxBytes {
		return fail("too large (%d bytes, limit %d)", len(raw), verifyConfigMaxBytes)
	}
	// Checked before decoding so a non-object file gets a message about the
	// schema instead of the decoder's Go type name.
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return fail("must contain a single JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cfg golemWorkspaceConfig
	if err := dec.Decode(&cfg); err != nil {
		return fail("invalid JSON: %v", err)
	}
	// Decode stops at the first value; anything after it means the file is not
	// the single object it claims to be.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fail("unexpected trailing content after the top-level object")
	}
	if cfg.Verify == nil {
		return nil, nil
	}
	if err := validateVerifySpec(cfg.Verify); err != nil {
		return fail("verify: %v", err)
	}
	return cfg.Verify, nil
}

// validateVerifySpec enforces everything the workspace is allowed to say. The
// executable itself is resolved later, by the tools layer, which also re-checks
// containment; these checks exist so a bad declaration is reported once at
// startup with a message naming the field.
func validateVerifySpec(s *verifySpec) error {
	if len(s.Argv) == 0 {
		return errors.New("argv is required and must be non-empty")
	}
	if strings.TrimSpace(s.Argv[0]) == "" {
		return errors.New("argv[0] must not be blank")
	}
	if len(s.Argv) > verifyMaxArgv {
		return fmt.Errorf("argv has %d entries, limit %d", len(s.Argv), verifyMaxArgv)
	}
	total := 0
	for _, a := range s.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return errors.New("argv must not contain NUL bytes")
		}
		total += len(a)
	}
	if total > verifyMaxArgvBytes {
		return fmt.Errorf("argv is %d bytes, limit %d", total, verifyMaxArgvBytes)
	}
	if s.Dir != "" {
		if filepath.IsAbs(s.Dir) {
			return fmt.Errorf("dir %q must be relative to the workspace root", s.Dir)
		}
		clean := filepath.Clean(filepath.FromSlash(s.Dir))
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("dir %q escapes the workspace", s.Dir)
		}
	}
	if s.TimeoutSeconds != nil && (*s.TimeoutSeconds < 1 || *s.TimeoutSeconds > verifyMaxTimeoutSeconds) {
		return fmt.Errorf("timeout_seconds must be between 1 and %d, got %d",
			verifyMaxTimeoutSeconds, *s.TimeoutSeconds)
	}
	return nil
}

// truncateMessage bounds a diagnostic that quotes workspace-controlled text.
// The cut lands on a rune boundary so an escaped sequence is never split.
func truncateMessage(s string) string {
	if len(s) <= verifyMessageMaxBytes {
		return s
	}
	end := verifyMessageMaxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "… [truncated]"
}
