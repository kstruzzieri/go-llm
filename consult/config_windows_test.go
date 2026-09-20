package consult

import (
	"context"
	"errors"
	"os"
	"strings"
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

func TestWindowsCodexUnsupported(t *testing.T) {
	for _, transport := range []string{"", "exec", "app-server"} {
		t.Run(transport, func(t *testing.T) {
			body := strings.Replace(codexConfig, `"adapter":"codex"`, `"adapter":"codex","transport":"`+transport+`"`, 1)
			path, _ := writeConfig(t, body)
			cs, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			r, err := Run(context.Background(), cs["codex"], "q")
			var ce *Error
			if !errors.As(err, &ce) || *ce != (Error{Code: "unsupported-platform", Reason: "platform"}) || r != (Receipt{}) {
				t.Fatalf("Codex Windows Run(%q) = %+v, %v", transport, r, err)
			}
		})
	}
}
