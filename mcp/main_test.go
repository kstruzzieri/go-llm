package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "go-llm-mcp-test-home-*")
	if err != nil {
		panic(err)
	}

	if err := os.Unsetenv("GO_LLM_CONFIG"); err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config")); err != nil {
		panic(err)
	}

	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
