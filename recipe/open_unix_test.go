//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package recipe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A subprocess bounds the test even if a regression blocks before f.Stat.
func TestOpenRecipeFileRejectsFIFOWithoutWriter(t *testing.T) {
	if os.Getenv("RECIPE_FIFO_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenRecipeFileRejectsFIFOWithoutWriter$")
		cmd.Env = append(os.Environ(), "RECIPE_FIFO_CHILD=1", "RECIPE_FIFO_PATH="+filepath.Join(t.TempDir(), "recipe.json"))
		output, err := cmd.CombinedOutput()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("opening the replaced FIFO blocked waiting for a writer")
		}
		if err != nil {
			t.Fatalf("FIFO subprocess: %v\n%s", err, output)
		}
		return
	}

	path := os.Getenv("RECIPE_FIFO_PATH")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	// Deterministically replace the regular target between Load's stat and open.
	f, err := openRecipeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := loadOpened(f, before)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("opened FIFO error = %v, want regular-file rejection", err)
	}
	if !reflect.DeepEqual(got, Recipe{}) {
		t.Errorf("opened FIFO returned nonzero recipe: %#v", got)
	}
}
