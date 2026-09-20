//go:build unix

package consult

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func fakeCodex(t *testing.T, transcript string) (Consultant, string) {
	t.Helper()
	dir := realTempDir(t)
	for name, data := range map[string]string{"version": "codex-cli 0.153.4\n", "stream": transcript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	body := fmt.Sprintf(`printf '%%s\n' "$1" >> %s
env > "%s.$1"
if [ "$1" = --version ]; then
 cat > %s
 cat %s
 exit 0
fi
printf '%%s\n' "$@" > %s
cat > %s
cat %s
`, shellQuote(filepath.Join(dir, "starts")), filepath.Join(dir, "env"), shellQuote(filepath.Join(dir, "version-stdin")), shellQuote(filepath.Join(dir, "version")), shellQuote(filepath.Join(dir, "args")), shellQuote(filepath.Join(dir, "stdin")), shellQuote(filepath.Join(dir, "stream")))
	c := consultantFor(t, body)
	c.Name, c.Adapter, c.Model = "codex", "codex", "gpt-6-astra"
	c.TrustedVendorRuntime = true
	return c, dir
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func appendFake(t *testing.T, c Consultant, tail string) {
	t.Helper()
	s := readTestFile(t, c.Command)
	if err := os.WriteFile(c.Command, []byte(s+tail), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestRunCodexReceipt(t *testing.T) {
	c, dir := fakeCodex(t, success)
	r, err := Run(context.Background(), c, "exact\nstdin\n")
	if err != nil {
		t.Fatal(err)
	}
	if r.Answer != "OK" || r.Adapter != "codex" || r.Consultant != "codex" || r.Version != "0.153.4" || r.Model != "gpt-6-astra" || r.ExitCode != 0 || r.Duration <= 0 || r.ContentForm != "consult-result/v1" || r.ContentSHA256 != "565339bc4d33d72817b583024112eb7f5cdf3e5eef0252d6ec1b9c9a94e12bb3" {
		t.Fatalf("Run(codex) = %+v", r)
	}
	want := Evidence{WaitStatus: "exited(0)", WaitErrorKind: "none", TrustedVendorRuntime: true, CodexInputTokens: 7, CodexCachedInputTokens: 2, CodexCacheWriteInputTokens: 0, CodexOutputTokens: 3, CodexReasoningOutputTokens: 1}
	if r.Evidence != want {
		t.Fatalf("Codex evidence = %+v, want %+v", r.Evidence, want)
	}
	for file, want := range map[string]string{"starts": "--version\nexec\n", "version-stdin": "", "stdin": "exact\nstdin\n"} {
		if got := readTestFile(t, filepath.Join(dir, file)); got != want {
			t.Fatalf("%s = %q, want %q", file, got, want)
		}
	}
	for _, phase := range []string{"--version", "exec"} {
		env := readTestFile(t, filepath.Join(dir, "env."+phase))
		if !strings.Contains(env, "CODEX_EXEC_SERVER_URL=none\n") || strings.Contains(env, "CLAUDE_") || strings.Contains(env, "ENABLE_CLAUDEAI") {
			t.Fatalf("%s environment = %q", phase, env)
		}
	}
	// Compare the launch with the independent literal argv in TestCodexArgs too.
	if got := readTestFile(t, filepath.Join(dir, "args")); got != strings.Join(codexArgs("gpt-6-astra"), "\n")+"\n" {
		t.Fatalf("launch argv = %q", got)
	}
	c, _ = fakeCodex(t, strings.Replace(success, `"cache_write_input_tokens":0`, `"cache_write_input_tokens":4`, 1))
	r, err = Run(context.Background(), c, "q")
	if err != nil || r.Evidence.CodexCacheWriteInputTokens != 4 {
		t.Fatalf("cache write usage = %+v, %v", r.Evidence, err)
	}
}

func TestRunCodexRejectsInvalidDeclaration(t *testing.T) {
	for name, change := range map[string]func(*Consultant){
		"runtime": func(c *Consultant) { c.TrustedVendorRuntime = false }, "egress": func(c *Consultant) { c.TrustedProcessEgress = false },
		"model": func(c *Consultant) { c.Model = "gpt-5.5" }, "adapter": func(c *Consultant) { c.Adapter = "unknown" },
		"name": func(c *Consultant) { c.Name = "bad name" }, "sha": func(c *Consultant) { c.SHA256 = "bad" },
		"timeout-zero": func(c *Consultant) { c.TimeoutSeconds = 0 }, "timeout-max": func(c *Consultant) { c.TimeoutSeconds = 301 },
		"cap-zero": func(c *Consultant) { c.MaxOutputBytes = 0 }, "cap-max": func(c *Consultant) { c.MaxOutputBytes = 1048577 },
	} {
		t.Run(name, func(t *testing.T) {
			c, dir := fakeCodex(t, success)
			change(&c)
			ce := mustFail(t, context.Background(), c, "q")
			if ce.Code != "input-invalid" || ce.Reason != "consultant" {
				t.Fatalf("invalid %s = %v", name, ce)
			}
			if _, err := os.Stat(filepath.Join(dir, "starts")); !os.IsNotExist(err) {
				t.Fatal("invalid declaration started process")
			}
		})
	}
}

func TestRunCodexVersions(t *testing.T) {
	for _, version := range []string{"codex-cli 0.153.5\n", "codex-cli 0.153.4", "codex-cli 0.153.4\r\n", " codex-cli 0.153.4\n", "codex-cli 0.153.4\nextra", "0.153.4\n"} {
		t.Run(strconv.Quote(version), func(t *testing.T) {
			c, dir := fakeCodex(t, success)
			if err := os.WriteFile(filepath.Join(dir, "version"), []byte(version), 0600); err != nil {
				t.Fatal(err)
			}
			ce := mustFail(t, context.Background(), c, "SECRET")
			if ce.Code != "unsupported-version" || ce.Reason != "version-mismatch" {
				t.Fatalf("version %q = %v", version, ce)
			}
			if got := readTestFile(t, filepath.Join(dir, "starts")); got != "--version\n" {
				t.Fatalf("version failure starts = %q", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "stdin")); !os.IsNotExist(err) {
				t.Fatal("prompt sent after rejected version")
			}
		})
	}
}

func TestRunCodexErrors(t *testing.T) {
	actionStart := `{"type":"item.started","item":` + actionItems[0] + "}\n"
	actionComplete := strings.Replace(actionStart, "item.started", "item.completed", 1)
	for _, tc := range []struct{ name, data, tail, code, reason string }{
		{"malformed", success + "SECRET\n", "", "protocol", "codex-invalid-protocol"},
		{"action", threadLine + turnLine + `{"type":"item.started","item":` + actionItems[0] + "}\n", "", "tool-activity", "codex-visible-action"},
		{"action-lifecycle", threadLine + turnLine + actionStart + actionComplete + answerLine + doneLine, "", "tool-activity", "codex-visible-action"},
		{"vendor", threadLine + turnLine + `{"type":"error","message":"SECRET /private/auth token"}` + "\n", "", "protocol", "codex-vendor-error"},
		{"missing", threadLine + turnLine + answerLine, "", "protocol", "codex-terminal-missing"},
		{"empty", threadLine + turnLine + doneLine, "", "protocol", "codex-no-answer"},
		{"exit", success, "exit 3\n", "process-exit", "exited(3)"},
		{"drain", success, "sleep 30 & exit 3\n", "drain-incomplete", "wait-delay"},
		{"stderr", success, "head -c 5000 /dev/zero >&2\n", "output-limit", "cap"},
		{"stdout", strings.Repeat("x", 5000), "", "output-limit", "cap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dir := fakeCodex(t, tc.data)
			appendFake(t, c, tc.tail)
			c.MaxOutputBytes = 4096
			old := runWaitDelay
			t.Cleanup(func() { runWaitDelay = old })
			runWaitDelay = 100 * time.Millisecond
			ce := mustFail(t, context.Background(), c, "q")
			if ce.Code != tc.code || ce.Reason != tc.reason || strings.Contains(ce.Error(), "SECRET") {
				t.Fatalf("%s = %v, want %s/%s", tc.name, ce, tc.code, tc.reason)
			}
			if got := readTestFile(t, filepath.Join(dir, "starts")); got != "--version\nexec\n" {
				t.Fatalf("failure retried: %q", got)
			}
		})
	}
}

func TestRunCodexBindsVersionAndBoundsProbe(t *testing.T) {
	for _, mode := range []string{"drift", "cap", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			c, dir := fakeCodex(t, success)
			body := readTestFile(t, c.Command)
			code, reason := "target-drift", "target"
			switch mode {
			case "drift":
				body = strings.Replace(body, " exit 0", ` printf '\n# changed\n' >> "$0"`+"\n exit 0", 1)
			case "cap":
				if err := os.WriteFile(filepath.Join(dir, "version"), []byte(strings.Repeat("x", 4097)), 0600); err != nil {
					t.Fatal(err)
				}
				code, reason = "output-limit", "cap"
			case "deadline":
				body = strings.Replace(body, " exit 0", " sleep 30\n exit 0", 1)
				c.TimeoutSeconds = 1
				code, reason = "timeout", "deadline"
			}
			if err := os.WriteFile(c.Command, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			ce := mustFail(t, context.Background(), c, "SECRET")
			if ce.Code != code || ce.Reason != reason {
				t.Fatalf("probe %s = %v, want %s/%s", mode, ce, code, reason)
			}
			if got := readTestFile(t, filepath.Join(dir, "starts")); got != "--version\n" {
				t.Fatalf("probe failure started prompt: %q", got)
			}
		})
	}
}

func TestRunCodexCancel(t *testing.T) {
	c, dir := fakeCodex(t, success)
	appendFake(t, c, "exec sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(dir, "stdin")); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	t.Cleanup(func() { cancel(); <-done })
	ce := mustFail(t, ctx, c, "q")
	if ce.Code != "canceled" || ce.Reason != "caller" {
		t.Fatalf("cancel codex = %v", ce)
	}
	if got := readTestFile(t, filepath.Join(dir, "starts")); got != "--version\nexec\n" {
		t.Fatalf("canceled starts = %q", got)
	}
}

func TestRunCodexEnvironment(t *testing.T) {
	for _, key := range []string{"HOME", "USER", "OPENAI_API_KEY", "CODEX_HOME", "HTTPS_PROXY", "ANTHROPIC_API_KEY"} {
		t.Setenv(key, "POISON")
	}
	c, _ := fakeCodex(t, success)
	// Dump only the environment of the fake child, with no native auth access.
	e, err := newEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(e.root); err != nil {
			t.Errorf("remove fake envelope: %v", err)
		}
	})
	identity, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "HOME=" + identity.HomeDir, "TMPDIR=" + e.tmp, "USER=" + identity.Username, "XDG_CONFIG_HOME=" + e.config, "XDG_CACHE_HOME=" + e.cache, "XDG_STATE_HOME=" + e.state, "CODEX_EXEC_SERVER_URL=none"}
	got, err := codexEnv(e)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("codexEnv = %v, %v; want %v", got, err, want)
	}
	out, err := run(context.Background(), runSpec{adapter: "codex", command: script(t, `env`), timeout: time.Second, outputCap: 4096})
	if err != nil || strings.Contains(string(out.Stdout), "POISON") || !strings.Contains(string(out.Stdout), "CODEX_EXEC_SERVER_URL=none\n") || strings.Contains(string(out.Stdout), "CLAUDE_") {
		t.Fatalf("Codex child environment = %q, %v", out.Stdout, err)
	}
	out, err = run(context.Background(), runSpec{adapter: "unknown", command: c.Command, timeout: time.Second, outputCap: 4096})
	if err == nil || len(out.Stdout) != 0 {
		t.Fatal("unknown private profile launched")
	}
}

func TestRunClaudeLeavesCodexEvidenceZero(t *testing.T) {
	c := fakeClaude(t, strings.Join([]string{validInit, assistantOK, goodResult}, "\n"), 0)
	c.TrustedVendorRuntime = true
	r, err := Run(context.Background(), c, "q")
	if err != nil {
		t.Fatal(err)
	}
	e := r.Evidence
	if e.TrustedVendorRuntime || e.CodexInputTokens != 0 || e.CodexCachedInputTokens != 0 || e.CodexCacheWriteInputTokens != 0 || e.CodexOutputTokens != 0 || e.CodexReasoningOutputTokens != 0 {
		t.Fatalf("Claude Codex-only evidence: %+v", e)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic") {
		t.Fatal("raw vendor identifiers retained")
	}
}

func TestRunCodexSharesDeadline(t *testing.T) {
	c, dir := fakeCodex(t, success)
	c.TimeoutSeconds = 2
	body := readTestFile(t, c.Command)
	body = strings.Replace(body, " exit 0", " sleep 0.9\n exit 0", 1) + "sleep 1.2\n"
	if err := os.WriteFile(c.Command, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	ce := mustFail(t, context.Background(), c, "q")
	if ce.Code != "timeout" || ce.Reason != "deadline" {
		t.Fatalf("shared deadline = %v", ce)
	}
	if got := readTestFile(t, filepath.Join(dir, "starts")); got != "--version\nexec\n" {
		t.Fatalf("shared deadline starts = %q", got)
	}
}

func TestRunCodexRejectsBeforePrompt(t *testing.T) {
	for _, mode := range []string{"pin", "target", "input", "utf8", "small-version-cap"} {
		t.Run(mode, func(t *testing.T) {
			c, dir := fakeCodex(t, success)
			prompt := "q"
			code, reason := "target-drift", "target"
			starts := ""
			switch mode {
			case "pin":
				c.SHA256 = strings.Repeat("0", 64)
			case "target":
				c.Command = filepath.Join(dir, "missing")
				code = "target-invalid"
			case "input":
				prompt = strings.Repeat("x", 65537)
				code, reason = "input-invalid", "prompt"
			case "utf8":
				prompt = "bad\xff"
				code, reason = "input-invalid", "prompt"
			case "small-version-cap":
				c.MaxOutputBytes = 10
				code, reason = "output-limit", "cap"
				starts = "--version\n"
			}
			ce := mustFail(t, context.Background(), c, prompt)
			if ce.Code != code || ce.Reason != reason {
				t.Fatalf("%s = %v, want %s/%s", mode, ce, code, reason)
			}
			b, err := os.ReadFile(filepath.Join(dir, "starts"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if string(b) != starts {
				t.Fatalf("%s starts=%q want %q", mode, b, starts)
			}
		})
	}
}

func TestRunCodexDurationIncludesPreflight(t *testing.T) {
	c, _ := fakeCodex(t, success)
	body := strings.Replace(readTestFile(t, c.Command), " exit 0", " sleep 0.5\n exit 0", 1)
	if err := os.WriteFile(c.Command, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), c, "q")
	if err != nil || r.Duration < 500*time.Millisecond {
		t.Fatalf("receipt duration = %v, %v; want at least preflight's 500ms", r.Duration, err)
	}
}
