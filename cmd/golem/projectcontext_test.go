package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigDirBaseXDGAbsolute(t *testing.T) {
	getenv := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/abs/config"
		}
		return ""
	}
	base, err := configDirBase(getenv)
	if err != nil {
		t.Fatalf("configDirBase: %v", err)
	}
	if base != "/abs/config" {
		t.Fatalf("base=%q, want /abs/config", base)
	}
}

func TestConfigDirBaseHomeFallback(t *testing.T) {
	getenv := func(k string) string {
		if k == "HOME" {
			return "/home/keith"
		}
		return ""
	}
	base, err := configDirBase(getenv)
	if err != nil {
		t.Fatalf("configDirBase: %v", err)
	}
	if base != "/home/keith/.config" {
		t.Fatalf("base=%q, want /home/keith/.config", base)
	}
}

func TestLoadProjectContextRequiresConfigLocation(t *testing.T) {
	t.Parallel()
	_, err := loadProjectContextDocs(context.Background(), t.TempDir(), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "cannot locate config dir") {
		t.Fatalf("loadProjectContextDocs(no config location) error = %v, want explicit config-dir error", err)
	}
}

func TestLoadProjectContextPropagatesCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	home := t.TempDir()
	_, err := loadProjectContextDocs(ctx, t.TempDir(), func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("loadProjectContextDocs(canceled) error = %v, want context canceled", err)
	}
}

func TestProjectContextLoadTimeout(t *testing.T) {
	t.Parallel()
	if projectContextLoadTimeout != 2*time.Second {
		t.Fatalf("projectContextLoadTimeout = %v, want %v", projectContextLoadTimeout, 2*time.Second)
	}
}

func TestLoadProjectContextAppendsWorkspaceDoc(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("prefer go test ./..."), 0o600); err != nil {
		t.Fatal(err)
	}
	// HOME points at an empty dir so no global doc exists.
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	docs, err := loadProjectContextDocs(context.Background(), root, getenv)
	if err != nil {
		t.Fatalf("loadProjectContextDocs: %v", err)
	}
	n, block := len(docs), projectContextInputs(systemInputs{}, docs, gitContextSnapshot{}, true).projectContext
	if n != 1 {
		t.Fatalf("want 1 doc, got %d", n)
	}
	if !strings.Contains(block, "prefer go test ./...") {
		t.Fatalf("block missing workspace content: %q", block)
	}
}

// loadProjectContextDocs must discover the global file under <config>/golem and label
// it "global" — this exercises the golem-specific config-base + "golem" namespace
// join that the library layer does not own.
func TestLoadProjectContextDiscoversGlobalDoc(t *testing.T) {
	cfg := t.TempDir()
	golemDir := filepath.Join(cfg, "golem")
	if err := os.MkdirAll(golemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(golemDir, "AGENTS.md"), []byte("global house rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir() // empty workspace: no workspace doc
	getenv := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return cfg
		}
		return ""
	}
	docs, err := loadProjectContextDocs(context.Background(), root, getenv)
	if err != nil {
		t.Fatalf("loadProjectContextDocs: %v", err)
	}
	n, block := len(docs), projectContextInputs(systemInputs{}, docs, gitContextSnapshot{}, true).projectContext
	if n != 1 {
		t.Fatalf("want 1 global doc, got %d", n)
	}
	if !strings.Contains(block, "global house rules") || !strings.Contains(block, "source=global") {
		t.Fatalf("block missing global doc/label: %q", block)
	}
}

func TestLoadProjectContextEmptyWhenNoFiles(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	docs, err := loadProjectContextDocs(context.Background(), root, getenv)
	if err != nil {
		t.Fatalf("loadProjectContextDocs: %v", err)
	}
	n, block := len(docs), projectContextInputs(systemInputs{}, docs, gitContextSnapshot{}, true).projectContext
	if n != 0 || block != "" {
		t.Fatalf("want empty block/0 docs, got n=%d block=%q", n, block)
	}
}
