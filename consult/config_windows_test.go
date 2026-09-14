package consult

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestWindowsLoadsWritableCommandThenReportsUnsupported(t *testing.T) {
	path, command := writeConfig(t, goodConfig)
	info, err := os.Stat(command)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o022 == 0 {
		t.Fatal("fixture must report Windows writable mode bits")
	}
	cs, err := Load(path)
	if err != nil {
		t.Fatalf("ordinary writable Windows file rejected during loading: %v", err)
	}
	r, err := Run(context.Background(), cs["claude"], "question\n")
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != "unsupported-platform" || r.Answer != "" {
		t.Fatalf("Windows run = %+v, %v; want unsupported-platform without a receipt", r, err)
	}
}
