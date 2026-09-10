package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/projectcontext"
)

// fenceSentinel matches a triple-angle-bracket fence lead (<<< or >>>) immediately
// followed by either injected-context sentinel, PROJECT_CONTEXT or GIT_CONTEXT
// (#354), case-insensitively. neutralizeFence inserts a space at the match so
// untrusted content cannot reproduce EITHER block's real open or close marker —
// including a forged, partial, or case-varied open marker that does not match
// the full open constant verbatim. One regex serves both blocks so a project
// file cannot forge a Git boundary and a branch name cannot forge a project one.
var fenceSentinel = regexp.MustCompile(`(?i)(<<<|>>>)(PROJECT_CONTEXT|GIT_CONTEXT)`)

// configDirBase resolves the per-user config base ($XDG_CONFIG_HOME if absolute,
// else $HOME/.config). A relative XDG_CONFIG_HOME is ignored; a relative or
// missing HOME with no usable XDG is an error. Mirrors session.go's dataDirBase
// but for config rather than data.
func configDirBase(getenv func(string) string) (string, error) {
	dir := getenv("XDG_CONFIG_HOME")
	relativeXDG := dir != "" && !filepath.IsAbs(dir)
	if relativeXDG {
		dir = ""
	}
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			if relativeXDG {
				return "", fmt.Errorf("golem: cannot locate config dir (XDG_CONFIG_HOME is relative and HOME unset)")
			}
			return "", fmt.Errorf("golem: cannot locate config dir (HOME and XDG_CONFIG_HOME unset)")
		}
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("golem: cannot locate config dir (HOME is relative)")
		}
		dir = filepath.Join(home, ".config")
	}
	return dir, nil
}

// neutralizeFence defangs any fence sentinel inside untrusted content so a project
// file or repository string cannot close an injected block early or forge a new
// boundary. It matches on the lead sentinel (<<<PROJECT_CONTEXT / >>>PROJECT_CONTEXT
// and <<<GIT_CONTEXT / >>>GIT_CONTEXT, case-insensitive) and inserts a space,
// breaking the literal marker the model keys on. A space-inserted replacement is
// sufficient: the model never needs to reconstruct the original bytes.
func neutralizeFence(s string) string {
	return fenceSentinel.ReplaceAllString(s, "$1 $2")
}

func truncateProjectContextPrefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end-- // back up to a UTF-8 rune boundary so we never emit a split rune
	}
	return s[:end]
}

// projectContextMaxBytes bounds the AGGREGATE rendered document body golem injects
// into the system prompt on every turn (across all discovered docs combined). It is
// a Golem-side prompt-injection budget, distinct from — and tighter than — the
// library's per-file default: project context ships as System on every turn, so an
// oversized AGENTS.md must not crowd out history, retrieval, and tool observations.
const projectContextMaxBytes = 16 * 1024

const projectContextLoadTimeout = 2 * time.Second

// loadProjectContextDocs discovers the bounded project-context documents for
// the workspace at root plus the per-user global config dir. Exact consent needs
// the complete candidate set, so config discovery and selected-file reads are strict.
func loadProjectContextDocs(ctx context.Context, root string, getenv func(string) string) ([]projectcontext.Document, error) {
	base, err := configDirBase(getenv)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, projectContextLoadTimeout)
	defer cancel()
	loader := &projectcontext.Loader{
		WorkspaceRoot: root,
		GlobalDir:     filepath.Join(base, "golem"),
		// Retain only a bounded prefix while hashing the complete file.
		MaxBytes: projectContextMaxBytes,
		Strict:   true,
	}
	return loader.Load(ctx)
}
