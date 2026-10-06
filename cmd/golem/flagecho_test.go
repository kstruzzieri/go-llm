package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// flagEchoSecret stands in for a credential pasted onto a subcommand's command
// line. No error, stream, or exit path may repeat it.
const flagEchoSecret = "SECRET-FLAGECHO"

// subcommandFlagSurface is one subcommand that parses its flags through
// parseQuietly. The want* templates replace MSG with the value-free parse
// failure; out/errOut are the in-process streams, stdout/stderr what the golem
// process prints once main() has rendered the returned error.
type subcommandFlagSurface struct {
	argv           []string // subcommand words before the flags
	golden         string   // testdata/usage/<golden>.golden holds its -h output
	typed          string   // a typed flag with an invalid value; "" when it has none
	call           func(args []string, out, errOut io.Writer) error
	out, errOut    string
	stdout, stderr string
}

func subcommandFlagSurfaces() []subcommandFlagSurface {
	ctx := context.Background()
	source := func(command string) func([]string, io.Writer, io.Writer) error {
		return func(args []string, out, errOut io.Writer) error {
			return runSource(ctx, append([]string{command}, args...), strings.NewReader(""), out, errOut)
		}
	}
	sourceSurface := func(command, typed string) subcommandFlagSurface {
		line := "golem source " + command + ": MSG\n"
		return subcommandFlagSurface{
			argv: []string{"source", command}, golden: "source-" + command, typed: typed, call: source(command),
			errOut: line, stderr: line,
		}
	}
	return []subcommandFlagSurface{
		{
			argv: []string{"index"}, golden: "index", typed: "-full=" + flagEchoSecret,
			call:   func(args []string, out, errOut io.Writer) error { return runIndex(ctx, args, out, errOut) },
			errOut: "golem index: MSG\n", stderr: "golem index: MSG\n",
		},
		{
			argv: []string{"models"}, golden: "models", typed: "-json=" + flagEchoSecret,
			call:   func(args []string, out, errOut io.Writer) error { return runModels(ctx, args, out, errOut) },
			stderr: "golem: golem models: MSG\n",
		},
		{
			argv: []string{"ops"}, golden: "ops", typed: "-json=" + flagEchoSecret,
			call: func(args []string, out, errOut io.Writer) error {
				return runOps(ctx, args, strings.NewReader(""), out, errOut)
			},
			stderr: "golem: golem ops: MSG\n",
		},
		sourceSurface("add", "-text="+flagEchoSecret),
		sourceSurface("list", "-json="+flagEchoSecret),
		// rm and reindex declare only string and repeatable flags, which accept
		// any value, so they have no invalid typed value to supply.
		sourceSurface("rm", ""),
		sourceSurface("reindex", ""),
	}
}

func flagParseFailure(n int) string {
	return fmt.Sprintf("invalid command-line flags in %d argument(s); run with -help for usage", n)
}

// TestSubcommandFlagErrorsDoNotEchoArgv pins every flag-package error form
// that quotes argv (unknown flag, double-dash, bad syntax, invalid typed value)
// on each subcommand, in process and through main().
func TestSubcommandFlagErrorsDoNotEchoArgv(t *testing.T) {
	for _, s := range subcommandFlagSurfaces() {
		vectors := [][]string{
			{"-sk-" + flagEchoSecret},
			{"--sk-" + flagEchoSecret},
			{"---sk-" + flagEchoSecret},
			{"-root", ".", "-sk-" + flagEchoSecret},
		}
		if s.typed != "" {
			vectors = append(vectors, []string{s.typed})
		}
		for _, args := range vectors {
			t.Run(strings.Join(append(slices.Clone(s.argv), args...), " "), func(t *testing.T) {
				fill := func(tpl string) string { return strings.ReplaceAll(tpl, "MSG", flagParseFailure(len(args))) }

				var out, errOut bytes.Buffer
				err := s.call(args, &out, &errOut)
				if err == nil || errors.Is(err, flag.ErrHelp) {
					t.Fatalf("error = %v, want a parse failure", err)
				}
				if strings.Contains(err.Error()+out.String()+errOut.String(), flagEchoSecret) {
					t.Fatalf("echoed argv: err=%q out=%q errOut=%q", err, out.String(), errOut.String())
				}
				if out.String() != fill(s.out) || errOut.String() != fill(s.errOut) {
					t.Fatalf("out/errOut = %q / %q, want %q / %q", out.String(), errOut.String(), fill(s.out), fill(s.errOut))
				}

				// The process view also catches parser output that bypasses the
				// injected writers and lands on os.Stderr.
				exit, stdout, stderr := runGolemMain(t, append(slices.Clone(s.argv), args...)...)
				if strings.Contains(stdout+stderr, flagEchoSecret) {
					t.Fatalf("process echoed argv: stdout=%q stderr=%q", stdout, stderr)
				}
				if exit != 1 || stdout != fill(s.stdout) || stderr != fill(s.stderr) {
					t.Fatalf("exit/stdout/stderr = %d / %q / %q, want 1 / %q / %q", exit, stdout, stderr, fill(s.stdout), fill(s.stderr))
				}
			})
		}
	}
}

// TestSubcommandHelpMatchesGolden pins each subcommand's help bytes, which
// predate parseQuietly, in process and through main().
func TestSubcommandHelpMatchesGolden(t *testing.T) {
	for _, s := range subcommandFlagSurfaces() {
		want, err := os.ReadFile(filepath.Join("testdata", "usage", s.golden+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		for _, help := range []string{"-h", "-help", "--help"} {
			t.Run(strings.Join(append(slices.Clone(s.argv), help), " "), func(t *testing.T) {
				var out, errOut bytes.Buffer
				if err := s.call([]string{help}, &out, &errOut); !errors.Is(err, flag.ErrHelp) {
					t.Fatalf("error = %v, want flag.ErrHelp", err)
				}
				if out.Len() != 0 || errOut.String() != string(want) {
					t.Fatalf("out/errOut = %q / %q, want \"\" / %q", out.String(), errOut.String(), want)
				}
				exit, stdout, stderr := runGolemMain(t, append(slices.Clone(s.argv), help)...)
				if exit != 0 || stdout != "" || stderr != string(want) {
					t.Fatalf("exit/stdout/stderr = %d / %q / %q, want 0 / \"\" / %q", exit, stdout, stderr, want)
				}
			})
		}
	}
}

// TestSourcePositionalErrorsDoNotEchoArgv covers the source paths that reject
// argv after flag parsing: an unknown subcommand and a flag after the
// positional argument.
func TestSourcePositionalErrorsDoNotEchoArgv(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-sk-" + flagEchoSecret}, "golem source: unknown source command\n" + sourceUsage},
		{[]string{"add", "notes.md", "-sk-" + flagEchoSecret}, "golem source add: flags must come before the positional argument\n"},
		{[]string{"rm", id, "--sk-" + flagEchoSecret}, "golem source rm: flags must come before the positional argument\n"},
		{[]string{"reindex", id, "---sk-" + flagEchoSecret}, "golem source reindex: flags must come before the positional argument\n"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runSource(context.Background(), tc.args, strings.NewReader(""), &out, &errOut)
			if !errors.Is(err, errSourceFailed) || out.Len() != 0 || errOut.String() != tc.want {
				t.Fatalf("err/out/errOut = %v / %q / %q, want errSourceFailed / \"\" / %q", err, out.String(), errOut.String(), tc.want)
			}
			exit, stdout, stderr := runGolemMain(t, append([]string{"source"}, tc.args...)...)
			if exit != 1 || stdout != "" || stderr != tc.want {
				t.Fatalf("exit/stdout/stderr = %d / %q / %q, want 1 / \"\" / %q", exit, stdout, stderr, tc.want)
			}
		})
	}
}

// TestUnknownCommandDoesNotEchoArgv covers the top-level dispatcher, which
// rejects a non-flag first argument before any flag parsing.
func TestUnknownCommandDoesNotEchoArgv(t *testing.T) {
	exit, stdout, stderr := runGolemMain(t, "sk-"+flagEchoSecret)
	want := "golem: unknown command (did you mean \"audit\", \"index\", \"models\", \"ops\", \"source\", or \"mcp\"?)\n"
	if exit != 1 || stdout != "" || stderr != want {
		t.Fatalf("exit/stdout/stderr = %d / %q / %q, want 1 / \"\" / %q", exit, stdout, stderr, want)
	}
}
