//go:build linux || darwin

package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"golang.org/x/sys/unix"
)

// screenThrough feeds in through one stream of s and returns what it delivers.
func screenThrough(s *outputScreen, in string) string {
	var buf bytes.Buffer
	w := s.stream(&buf)
	_, _ = w.Write([]byte(in))
	w.finish()
	return buf.String()
}

// mustBuild builds the screen for ws and fails on an error.
func mustBuild(t *testing.T, label string, ws *Workspace) *outputScreen {
	t.Helper()
	s, err := buildOutputScreen(t.Context(), ws)
	if err != nil || s == nil {
		t.Fatalf("%s: build failed (error: %t)", label, err != nil)
	}
	return s
}

// failRead is a readFrom seam whose read of want fails with EIO and returns
// no content; every other path reads through.
func failRead(want string) func(string, io.Reader) io.Reader {
	return func(rel string, r io.Reader) io.Reader {
		if rel == want {
			return iotest.ErrReader(syscall.EIO)
		}
		return r
	}
}

// failingReadDir is a readDir seam whose read of dir emits .env, typed
// regular, and then fails with EIO; every other directory is read for real.
func failingReadDir(dir string) func(string, *os.File, func(string, uint8)) error {
	return func(rel string, f *os.File, emit func(string, uint8)) error {
		if rel != dir {
			return sysDir.read(f, emit)
		}
		emit(".env", unix.DT_REG)
		return syscall.EIO
	}
}

const withheldNote = "credential screen: output withheld; credential files could not be read completely ("

func TestBuildOutputScreenKeys(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	ws, _, _ := tolerantFixture(t, map[string]string{
		".env":             "TOKEN=env-0001-abcdef\n",
		"sub/.npmrc":       "//registry.example.invalid/:_authToken=npm-0002-abcdef\n",
		".env.example":     "TOKEN=example-0003-abcdef\n",
		".git/config":      "[remote \"origin\"]\n\turl = https://user-0004:pass-0004-abcdef@example.invalid/r.git\n",
		"vendor/x/.env":    "TOKEN=vendor-0005-abcdef\n",
		".git-credentials": "https://user-0006:pass-0006-abcdef@example.invalid\n",
	}, nil)
	var stats []string
	ws.screenSeams = &screenSeams{statErr: recordStats(&stats, nil)}
	s := mustBuild(t, "keys", ws)
	if s.notes() != nil {
		t.Fatalf("keys: unexpected notes (%d)", len(s.notes()))
	}
	// .env.example is a plain regular file and is never stat'ed;
	// vendor/x/.env is under an ignored directory and is never listed;
	// .git/config is openGitConfig's stat.
	slices.Sort(stats)
	wantStats := []string{".env", ".git", ".git-credentials", ".git/config", "sub", "sub/.npmrc", "vendor"}
	if !slices.Equal(stats, wantStats) {
		t.Fatalf("keys: stat'ed paths differ (%d, want %d)", len(stats), len(wantStats))
	}
	rows := []struct{ label, in, want string }{
		{"env", "TOKEN=env-0001-abcdef\n", tM + "\n"},
		{"env value", "got env-0001-abcdef\n", "got " + tM + "\n"},
		{"npmrc", "//registry.example.invalid/:_authToken=npm-0002-abcdef\n", tM + "\n"},
		{"npmrc value", "npm-0002-abcdef\n", tM + "\n"},
		{"git config", "[remote \"origin\"]\n\turl = https://user-0004:pass-0004-abcdef@example.invalid/r.git\n", tM + "\n\t" + tM + "\n"},
		{"git password", "pass-0004-abcdef\n", tM + "\n"},
		{"store password", "password=pass-0006-abcdef\n", "password=" + tM + "\n"},
		{"env example", "TOKEN=example-0003-abcdef\n", "TOKEN=example-0003-abcdef\n"},
		{"vendor", "TOKEN=vendor-0005-abcdef\n", "TOKEN=vendor-0005-abcdef\n"},
	}
	for _, r := range rows {
		if got := screenThrough(s, r.in); got != r.want {
			t.Errorf("%s: output differs (len %d, want %d)", r.label, len(got), len(r.want))
		}
	}
}

// A directory whose type the read did not report is stat'ed and descended,
// and the credential file inside it is keyed.
func TestBuildOutputScreenUnknownType(t *testing.T) {
	const label = "unknown type"
	ws, _, _ := tolerantFixture(t, map[string]string{"plain-dir/.env": "TOKEN=unknown-0010-abcdef\n"}, nil)
	ws.screenSeams = &screenSeams{entryType: func(rel string, typ uint8) uint8 {
		if rel == "plain-dir" {
			return unix.DT_UNKNOWN
		}
		return typ
	}}
	s := mustBuild(t, label, ws)
	if s.notes() != nil {
		t.Fatalf("%s: unexpected notes (%d)", label, len(s.notes()))
	}
	if got := screenThrough(s, "unknown-0010-abcdef\n"); got != tM+"\n" {
		t.Fatalf("%s: value not withheld (len %d)", label, len(got))
	}
}

// A name the directory read typed regular is not descended, even when it is
// a directory by the time the walk reaches it. These rows pin a stated limit
// (spec D1), not a guarantee.
func TestBuildOutputScreenStaleType(t *testing.T) {
	t.Run("typed regular", func(t *testing.T) {
		const label = "typed regular"
		ws, _, _ := tolerantFixture(t, map[string]string{"z/.env": "TOKEN=stale-0001-abcdef\n"}, nil)
		ws.screenSeams = &screenSeams{entryType: func(rel string, typ uint8) uint8 {
			if rel == "z" {
				return unix.DT_REG
			}
			return typ
		}}
		s := mustBuild(t, label, ws)
		if s.notes() != nil {
			t.Fatalf("%s: unexpected notes (%d)", label, len(s.notes()))
		}
		if got := screenThrough(s, "stale-0001-abcdef\n"); got != "stale-0001-abcdef\n" {
			t.Fatalf("%s: output changed (len %d)", label, len(got))
		}
	})
	t.Run("replaced", func(t *testing.T) {
		// z.txt is replaced by a directory holding a credential file after
		// the root was read and before z.txt is reached. The code at
		// 23d40b33 stats z.txt when it reaches it, descends that directory
		// and keys the file; this row pins the stated limit of spec D1, not
		// a guarantee.
		const label = "replaced"
		ws, root, sib := tolerantFixture(t, map[string]string{
			".env":  "TOKEN=stale-0002-abcdef\n",
			"z.txt": "x\n",
		}, map[string]string{".env": "TOKEN=stale-0003-abcdef\n"})
		ws.screenSeams = &screenSeams{statErr: func(rel string) error {
			if rel == ".env" {
				if err := os.Rename(filepath.Join(root, "z.txt"), root+"-z-away"); err != nil {
					failNoPath(t, label, err)
				}
				if err := os.Rename(sib, filepath.Join(root, "z.txt")); err != nil {
					failNoPath(t, label, err)
				}
			}
			return nil
		}}
		s := mustBuild(t, label, ws)
		if s.notes() != nil {
			t.Fatalf("%s: unexpected notes (%d)", label, len(s.notes()))
		}
		if got := screenThrough(s, "stale-0003-abcdef\n"); got != "stale-0003-abcdef\n" {
			t.Fatalf("%s: output changed (len %d)", label, len(got))
		}
	})
}

// A credential file the listing returned unexamined is never read: the
// output is withheld with a cause that names it, or the command is not run.
func TestBuildOutputScreenUnexamined(t *testing.T) {
	const secret = "unexamined-0001-abcdef"
	rows := []struct {
		label   string
		act     func(t *testing.T, root, sib string) // run when sub/.env is about to be opened
		wantErr error                                // non-nil: the build fails with it
	}{
		{label: "withheld"},
		{label: "parent moved", act: func(t *testing.T, root, _ string) {
			if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub-away")); err != nil {
				failNoPath(t, "parent moved", err)
			}
		}},
		{label: "root swapped", act: func(t *testing.T, root, sib string) { swapRoot(t, root, sib) }, wantErr: ErrRootReplaced},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			ws, root, sib := tolerantFixture(t, map[string]string{
				"sub/.env": "TOKEN=" + secret + "\n",
				"zz/f":     "x\n",
			}, map[string]string{"a": "x\n"})
			var asked []string
			ws.screenSeams = &screenSeams{
				examine: func(rel string, need bool) bool { return need && rel != "sub/.env" },
				openErr: func(rel string) error {
					asked = append(asked, rel)
					if rel == "sub/.env" && r.act != nil {
						r.act(t, root, sib)
					}
					return nil
				},
			}
			s, err := buildOutputScreen(t.Context(), ws)
			if r.wantErr != nil {
				if !errors.Is(err, r.wantErr) || s != nil {
					t.Fatalf("%s: wrong outcome (error: %t, screen: %t)", r.label, err != nil, s != nil)
				}
				secretFree(t, r.label, []string{secret}, err.Error())
				return
			}
			if err != nil || s == nil {
				t.Fatalf("%s: build failed (error: %t)", r.label, err != nil)
			}
			notes := s.notes()
			want := []string{withheldNote + `listed without metadata: "sub/.env")`}
			if !slices.Equal(notes, want) {
				t.Fatalf("%s: notes differ (%d notes)", r.label, len(notes))
			}
			secretFree(t, r.label, []string{secret}, notes...)
			if got := screenThrough(s, secret+"\n"); got != "" {
				t.Fatalf("%s: output delivered (len %d)", r.label, len(got))
			}
			if slices.Contains(asked, "zz") {
				t.Fatalf("%s: walk went on after the withhold", r.label)
			}
		})
	}
}

func TestBuildOutputScreenGitConfigAlias(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	ws, _, _ := tolerantFixture(t, map[string]string{
		".GIT/config": "[remote \"origin\"]\n\turl = https://user-0012:pass-0012-abcdef@example.invalid/r.git\n",
	}, nil)
	s := mustBuild(t, "alias", ws)
	if got := screenThrough(s, "pass-0012-abcdef\n"); got != tM+"\n" {
		t.Fatalf("alias: password delivered (len %d)", len(got))
	}
}

func TestBuildOutputScreenGitConfigRootReplaced(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	// .env sorts before .git, so .git is the last root entry.
	ws, root, sib := tolerantFixture(t, map[string]string{
		".env":        "TOKEN=env-0010-abcdef\n",
		".git/config": "[remote \"origin\"]\n\turl = https://user-0010:pass-0010-abcdef@example.invalid/r.git\n",
	}, map[string]string{".env": "TOKEN=env-0011-abcdef\n"})
	ws.screenSeams = &screenSeams{openErr: func(rel string) error {
		if rel != ".git/config" {
			return nil
		}
		swapRoot(t, root, sib)
		return fs.ErrNotExist // the ordinary error, not the sentinel
	}}
	s, err := buildOutputScreen(t.Context(), ws)
	if !errors.Is(err, ErrRootReplaced) || s != nil {
		t.Fatalf("git config root replaced: want ErrRootReplaced and no screen (error: %t, screen: %t)", err != nil, s != nil)
	}
}

func TestBuildOutputScreenGitConfigSymlink(t *testing.T) {
	ws, root, _ := tolerantFixture(t, map[string]string{
		"notes/remote.txt": "url = https://user-0013:pass-0013-abcdef@example.invalid/r.git\n",
		".git/HEAD":        "ref: refs/heads/main\n",
	}, nil)
	if err := os.Symlink("../notes/remote.txt", filepath.Join(root, ".git", "config")); err != nil {
		t.Fatal(err)
	}
	s := mustBuild(t, "symlink", ws)
	if s.notes() != nil {
		t.Fatalf("symlink: unexpected notes (%d)", len(s.notes()))
	}
	// Stated exclusion: a credential-named symlink is never read.
	if got := screenThrough(s, "pass-0013-abcdef\n"); got != "pass-0013-abcdef\n" {
		t.Fatalf("symlink: output changed (len %d)", len(got))
	}
}

func TestScreenCommonEnvValues(t *testing.T) {
	ws, _, _ := tolerantFixture(t, map[string]string{".env": "HOST=localhost\nNODE_ENV=development\n"}, nil)
	s := mustBuild(t, "common", ws)
	want := "Server at http://" + tM + ":3000 (" + tM + ")\n"
	if got := screenThrough(s, "Server at http://localhost:3000 (development)\n"); got != want {
		t.Fatalf("common: output differs (len %d, want %d)", len(got), len(want))
	}
}

func TestBuildOutputScreenWorktreeGitFile(t *testing.T) {
	ws, _, _ := tolerantFixture(t, map[string]string{".git": "gitdir: /elsewhere\n"}, nil)
	s := mustBuild(t, "worktree", ws)
	if s.notes() != nil {
		t.Fatalf("worktree: unexpected notes (%d)", len(s.notes()))
	}
	if got := screenThrough(s, "hello\n"); got != "hello\n" {
		t.Fatalf("worktree: output differs (len %d)", len(got))
	}
}

func TestBuildOutputScreenNonRegular(t *testing.T) {
	ws, root, _ := tolerantFixture(t, nil, nil)
	if err := unix.Mkfifo(filepath.Join(root, ".env"), 0o600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		s   *outputScreen
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := buildOutputScreen(context.Background(), ws)
		done <- result{s, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.s == nil || r.s.notes() != nil {
			t.Fatalf("fifo: want a screen with no notes (error: %t)", r.err != nil)
		}
		if got := screenThrough(r.s, "hello\n"); got != "hello\n" {
			t.Fatalf("fifo: output differs (len %d)", len(got))
		}
	case <-time.After(time.Second):
		t.Fatal("fifo: build did not return within one second")
	}
}

// secretFree fails when any of secrets appears in texts.
func secretFree(t *testing.T, label string, secrets []string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for i, k := range secrets {
			if strings.Contains(text, k) {
				t.Fatalf("%s: a note or error holds key %d", label, i)
			}
		}
	}
}

func TestBuildOutputScreenBounds(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	const (
		file   = "TOKEN=bound-0001-abcdef-0123456789\n"     // 35 bytes, one record of 34
		keys3  = "A=key-0003-aaaaaaaa\nkey-0003-cccccccc\n" // 3 keys
		total  = "TOKEN=bound-0004-abcdef\n"                // keys of 23 and 17 bytes
		budget = "TOKEN=bound-0005-abcdef\n"
	)
	rows := []struct {
		label  string
		env    string
		limits screenLimits
		slow   bool   // beforeReadDir sleeps 300 ms
		cause  string // "": not withheld
	}{
		{"file", file, screenLimits{fileBytes: 34}, false, `file over 34 bytes: ".env"`},
		{"file at limit", file, screenLimits{fileBytes: 35}, false, ""},
		{"record", file, screenLimits{keyBytes: 33}, false, `line over 33 bytes: ".env"`},
		{"record at limit", file, screenLimits{keyBytes: 34}, false, ""},
		{"keys", keys3, screenLimits{keys: 2}, false, "more than 2 keys"},
		{"keys at limit", keys3, screenLimits{keys: 3}, false, ""},
		{"total", total, screenLimits{totalBytes: 39}, false, "keys over 39 bytes"},
		{"total at limit", total, screenLimits{totalBytes: 40}, false, ""},
		{"budget", budget, screenLimits{budget: 10 * time.Millisecond}, true, "time budget of 10ms exhausted"},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			ws, _, _ := tolerantFixture(t, map[string]string{".env": r.env, "a.txt": "x\n"}, nil)
			ws.screenLimits = r.limits
			if r.slow {
				// Far past the 10 ms budget, so the budget's timer has fired
				// even on a loaded machine.
				ws.beforeReadDir = func(string) { time.Sleep(300 * time.Millisecond) }
			}
			s := mustBuild(t, r.label, ws)
			notes := s.notes()
			secretFree(t, r.label, strings.Split(strings.TrimSpace(r.env), "\n"), notes...)
			if r.cause == "" {
				if notes != nil {
					t.Fatalf("%s: unexpected notes (%d)", r.label, len(notes))
				}
				if got := screenThrough(s, "hello\n"); got != "hello\n" {
					t.Fatalf("%s: output differs (len %d)", r.label, len(got))
				}
				return
			}
			want := withheldNote + r.cause + ")"
			if len(notes) != 1 || notes[0] != want {
				t.Fatalf("%s: notes differ (%d notes)", r.label, len(notes))
			}
			if got := screenThrough(s, "hello\n"); got != "" {
				t.Fatalf("%s: output delivered (len %d)", r.label, len(got))
			}
		})
	}
	// The budget runs out while the last directory, an empty one, is
	// opened: the walk checks it on entering a directory, not only between
	// entries. The sleep is far past the budget so the budget's timer has
	// fired even on a loaded machine.
	t.Run("budget at empty dir", func(t *testing.T) {
		ws, root, _ := tolerantFixture(t, map[string]string{".env": budget, "a.txt": "x\n"}, nil)
		if err := os.Mkdir(filepath.Join(root, "z"), 0o755); err != nil {
			t.Fatal(err)
		}
		ws.screenLimits = screenLimits{budget: 10 * time.Millisecond}
		ws.screenSeams = &screenSeams{openErr: func(rel string) error {
			if rel == "z" {
				time.Sleep(300 * time.Millisecond)
			}
			return nil
		}}
		notes := mustBuild(t, "empty dir", ws).notes()
		if !slices.Equal(notes, []string{withheldNote + "time budget of 10ms exhausted)"}) {
			t.Fatalf("empty dir: notes differ (%d notes)", len(notes))
		}
	})
	// Two files hold the same three keys: a key already held is counted
	// once, so limits that fit one file's keys build a screen with them.
	for _, r := range []struct {
		label  string
		limits screenLimits
	}{
		{"keys repeated", screenLimits{keys: 3}},
		{"total repeated", screenLimits{totalBytes: 53}},
	} {
		t.Run(r.label, func(t *testing.T) {
			ws, _, _ := tolerantFixture(t, map[string]string{".env": keys3, "sub/.env": keys3}, nil)
			ws.screenLimits = r.limits
			s := mustBuild(t, r.label, ws)
			if notes := s.notes(); notes != nil {
				t.Fatalf("%s: unexpected notes (%d)", r.label, len(notes))
			}
			if got := screenThrough(s, "key-0003-cccccccc\n"); got != tM+"\n" {
				t.Fatalf("%s: output differs (len %d)", r.label, len(got))
			}
		})
	}
	// A .git/config line with more than 32 "=" after a name is not keyed:
	// it withholds with its own cause, not the line-length one.
	t.Run("crowded git config", func(t *testing.T) {
		ws, _, _ := tolerantFixture(t, map[string]string{".git/config": strings.Repeat("a=", 33) + "x\n"}, nil)
		s := mustBuild(t, "crowded", ws)
		want := withheldNote + `more than 32 name= pairs on a line: ".git/config")`
		if notes := s.notes(); len(notes) != 1 || notes[0] != want {
			t.Fatalf("crowded: notes differ (%d notes)", len(notes))
		}
		if got := screenThrough(s, "hello\n"); got != "" {
			t.Fatalf("crowded: output delivered (len %d)", len(got))
		}
	})
	t.Run("defaults", func(t *testing.T) {
		want := screenLimits{fileBytes: 262144, keyBytes: 16384, keys: 4096, totalBytes: 1048576, budget: 5 * time.Second}
		if got := (screenLimits{}).orDefaults(); got != want {
			t.Fatalf("default limits differ (budget %s)", got.budget)
		}
	})
}

func TestScreenNotePathIsQuoted(t *testing.T) {
	name := ".env.\nnote: forged"
	ws, _, _ := tolerantFixture(t, map[string]string{name: "TOKEN=quoted-0001-abcdef\n"}, nil)
	ws.screenLimits = screenLimits{fileBytes: 4}
	notes := mustBuild(t, "quoted", ws).notes()
	want := withheldNote + `file over 4 bytes: ".env.\nnote: forged")`
	if len(notes) != 1 || notes[0] != want || strings.ContainsAny(notes[0], "\n\r") {
		t.Fatalf("quoted: note differs (%d notes)", len(notes))
	}

	long := strings.Repeat("a", 300)
	if got := notePath(long); got != strings.Repeat("a", 200) {
		t.Fatalf("truncate: len %d, want 200", len(got))
	}
	if got := notePath("short"); got != "short" {
		t.Fatalf("short: len %d, want 5", len(got))
	}
}

func TestBuildOutputScreenSkips(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	tree := map[string]string{
		".env":       "TOKEN=skip-0001-abcdef\n",
		"data/.env":  "TOKEN=skip-0002-abcdef\n",
		"sub/.npmrc": "_authToken=skip-0003-abcdef\n",
		"z/.env":     "TOKEN=skip-0004-abcdef\n",
		"data/a.txt": "x\n",
	}
	secrets := []string{"skip-0001-abcdef", "skip-0002-abcdef", "skip-0003-abcdef", "skip-0004-abcdef"}
	changed := "credential screen: 1 entr(ies) changed during the read and were skipped; output screened without them"
	rows := []struct {
		label string
		seams func(t *testing.T, root string) *screenSeams
		want  string // the one note; "": no note at all
		// later: a value from z/.env, which sorts after data, still comes out as M
		later bool
	}{
		{"file denied", func(*testing.T, string) *screenSeams {
			return &screenSeams{openErr: answer(".env", fs.ErrPermission)}
		}, withheldNote + `unreadable: ".env")`, false},
		{"dir denied", func(*testing.T, string) *screenSeams {
			return &screenSeams{openErr: answer("data", fs.ErrPermission)}
		}, withheldNote + `unreadable: "data")`, false},
		{"dir gone", func(*testing.T, string) *screenSeams {
			return &screenSeams{openErr: answer("data", fs.ErrNotExist)}
		}, changed, true},
		{"file removed", func(t *testing.T, root string) *screenSeams {
			return &screenSeams{openErr: func(rel string) error {
				if rel == ".env" {
					if err := os.Remove(filepath.Join(root, ".env")); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}}
		}, changed, true},
		{"stat gone", func(*testing.T, string) *screenSeams {
			return &screenSeams{statErr: answer("sub/.npmrc", fs.ErrNotExist)}
		}, changed, true},
		{"read eio", func(*testing.T, string) *screenSeams {
			return &screenSeams{readFrom: failRead(".env")}
		}, withheldNote + `unreadable: ".env")`, false},
		{"stat eio", func(*testing.T, string) *screenSeams {
			return &screenSeams{statErr: answer("sub/.npmrc", syscall.EIO)}
		}, withheldNote + `unreadable: "sub/.npmrc")`, false},
		{"list eio", func(*testing.T, string) *screenSeams {
			return &screenSeams{listErr: answer("sub", syscall.EIO)}
		}, withheldNote + `listing failed: "sub")`, false},
		// A plain file is never stat'ed, so its stat seam's error never shows.
		{"plain stat eio", func(*testing.T, string) *screenSeams {
			return &screenSeams{statErr: answer("data/a.txt", syscall.EIO)}
		}, "", true},
		// A read error after valid records is a failed listing; the records
		// read before it are not used.
		{"read error subdir", func(*testing.T, string) *screenSeams {
			return &screenSeams{readDir: failingReadDir("data")}
		}, withheldNote + `listing failed: "data")`, false},
		{"read twice", func(*testing.T, string) *screenSeams {
			return &screenSeams{readDir: func(rel string, f *os.File, emit func(string, uint8)) error {
				if rel != "" {
					return sysDir.read(f, emit)
				}
				return sysDir.read(f, func(name string, typ uint8) {
					emit(name, typ)
					emit(name, typ)
				})
			}}
		}, "", true},
		// A name typed regular that is gone by the time it would be used is
		// unexamined, and a plain name is never opened.
		{"plain gone", func(*testing.T, string) *screenSeams {
			return &screenSeams{readDir: func(rel string, f *os.File, emit func(string, uint8)) error {
				if err := sysDir.read(f, emit); err != nil || rel != "" {
					return err
				}
				emit("ghost.txt", unix.DT_REG)
				return nil
			}}
		}, "", true},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			ws, root, _ := tolerantFixture(t, tree, nil)
			ws.screenSeams = r.seams(t, root)
			var asked []string
			inner := ws.screenSeams.openErr
			ws.screenSeams.openErr = func(rel string) error {
				asked = append(asked, rel)
				if inner != nil {
					return inner(rel)
				}
				return nil
			}
			s := mustBuild(t, r.label, ws)
			// A withheld run delivers no credential, whatever else differs.
			if !r.later {
				if got := screenThrough(s, secrets[0]+"\n"); got != "" {
					t.Errorf("%s: credential delivered (len %d)", r.label, len(got))
				}
			}
			// A withhold ends the walk: z, which sorts last, is never opened.
			if !r.later && slices.Contains(asked, "z") {
				t.Fatalf("%s: walk went on after the withhold", r.label)
			}
			notes := s.notes()
			var wantNotes []string
			if r.want != "" {
				wantNotes = []string{r.want}
			}
			if !slices.Equal(notes, wantNotes) {
				t.Fatalf("%s: notes differ (%d notes)", r.label, len(notes))
			}
			secretFree(t, r.label, secrets, notes...)
			out := screenThrough(s, "hello\n")
			if r.later {
				if out != "hello\n" {
					t.Fatalf("%s: output differs (len %d)", r.label, len(out))
				}
				if got := screenThrough(s, "skip-0004-abcdef\n"); got != tM+"\n" {
					t.Fatalf("%s: later key not withheld (len %d)", r.label, len(got))
				}
			} else if out != "" {
				t.Fatalf("%s: output delivered (len %d)", r.label, len(out))
			}
		})
	}
}

func TestBuildOutputScreenRootReplaced(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	var texts []string
	check := func(label string, s *outputScreen, err, want error) {
		t.Helper()
		if !errors.Is(err, want) || s != nil {
			t.Fatalf("%s: wrong outcome (error: %t, screen: %t)", label, err != nil, s != nil)
		}
		texts = append(texts, err.Error())
	}

	ws, root, sib := tolerantFixture(t, map[string]string{".env": "TOKEN=root-0001-abcdef\n"}, map[string]string{"a": "x\n"})
	ws.beforeReadDir = func(rel string) {
		if rel == "" {
			swapRoot(t, root, sib)
		}
	}
	s, err := buildOutputScreen(t.Context(), ws)
	check("swap", s, err, ErrRootReplaced)

	// The root is swapped while a credential file is opened, and the open
	// fails with an ordinary error: the walk has no later check that would
	// catch it, so the file's own failure must end the build.
	ws, root, sib = tolerantFixture(t, map[string]string{".env": "TOKEN=root-0005-abcdef\n"}, map[string]string{"a": "x\n"})
	ws.screenSeams = &screenSeams{openErr: func(rel string) error {
		if rel != ".env" {
			return nil
		}
		swapRoot(t, root, sib)
		return fs.ErrNotExist // the ordinary error, not the sentinel
	}}
	s, err = buildOutputScreen(t.Context(), ws)
	check("file open swap", s, err, ErrRootReplaced)

	ws, _, _ = tolerantFixture(t, map[string]string{".env": "TOKEN=root-0002-abcdef\n"}, nil)
	ws.screenSeams = &screenSeams{listErr: answer("", syscall.EIO)}
	s, err = buildOutputScreen(t.Context(), ws)
	check("root listing", s, err, errAnchorUnreachable)

	ws, _, _ = tolerantFixture(t, map[string]string{".env": "TOKEN=root-0007-abcdef\n"}, nil)
	ws.screenSeams = &screenSeams{readDir: failingReadDir("")}
	s, err = buildOutputScreen(t.Context(), ws)
	check("read error root", s, err, errAnchorUnreachable)

	ws, _, _ = tolerantFixture(t, map[string]string{".env": "TOKEN=root-0003-abcdef\n"}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s, err = buildOutputScreen(ctx, ws)
	check("canceled", s, err, context.Canceled)

	// A parent deadline is the parent's error, not the screen's budget.
	ws, _, _ = tolerantFixture(t, map[string]string{".env": "TOKEN=root-0004-abcdef\n", "a.txt": "x\n"}, nil)
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Hour))
	defer cancel()
	s, err = buildOutputScreen(ctx, ws)
	check("parent deadline", s, err, context.DeadlineExceeded)

	// The root is replaced after the time budget has run out: the walk ends
	// on the replaced root, which is not the budget, so the build fails. The
	// sleep is far past the budget so its timer has fired.
	ws, root, sib = tolerantFixture(t, map[string]string{".env": "TOKEN=root-0006-abcdef\n"}, map[string]string{"a": "x\n"})
	ws.screenLimits = screenLimits{budget: 10 * time.Millisecond}
	ws.beforeReadDir = func(rel string) {
		if rel == "" {
			time.Sleep(300 * time.Millisecond)
			swapRoot(t, root, sib)
		}
	}
	s, err = buildOutputScreen(t.Context(), ws)
	check("swap after budget", s, err, ErrRootReplaced)

	secretFree(t, "root", []string{"root-0001-abcdef", "root-0002-abcdef", "root-0003-abcdef", "root-0004-abcdef", "root-0005-abcdef", "root-0006-abcdef", "root-0007-abcdef"}, texts...)
}

// .git/config is read through the held .git descriptor, so a .git moved,
// replaced or denied during the read is never silently "no config".
func TestBuildOutputScreenGitConfigHeld(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	const cfg = "[remote \"origin\"]\n\turl = https://user-0020:pass-0020-abcdef@example.invalid/r.git\n"
	gitTree := map[string]string{".git/config": cfg, "a.txt": "x\n"}
	changed := "credential screen: 1 entr(ies) changed during the read and were skipped; output screened without them"
	mustRename := func(t *testing.T, from, to string) {
		t.Helper()
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
	}
	rows := []struct {
		label   string
		tree    map[string]string
		setup   func(t *testing.T, ws *Workspace, root, sib string)
		wantErr error    // non-nil: the build fails with it
		notes   []string // the screen's notes
		keyed   bool     // the config's password comes out as M
	}{
		{"git dir renamed away", gitTree, func(t *testing.T, ws *Workspace, root, _ string) {
			ws.screenSeams = &screenSeams{openErr: func(rel string) error {
				if rel == ".git" {
					mustRename(t, filepath.Join(root, ".git"), filepath.Join(root, ".git-x"))
				}
				return nil
			}}
		}, nil, []string{changed}, false},
		{"git dir away and back", gitTree, func(t *testing.T, ws *Workspace, root, _ string) {
			ws.screenSeams = &screenSeams{openErr: func(rel string) error {
				if rel == ".git" {
					mustRename(t, filepath.Join(root, ".git"), filepath.Join(root, ".git-x"))
					mustRename(t, filepath.Join(root, ".git-x"), filepath.Join(root, ".git"))
				}
				return nil
			}}
		}, nil, nil, true},
		{"no config", map[string]string{".git/HEAD": "ref: refs/heads/main\n"}, func(*testing.T, *Workspace, string, string) {}, nil, nil, false},
		{"git dir replaced", gitTree, func(t *testing.T, ws *Workspace, root, _ string) {
			other := root + "-other-git"
			writeTree(t, other, map[string]string{"config": "[core]\n"})
			ws.screenSeams = &screenSeams{openErr: func(rel string) error {
				if rel == ".git" {
					mustRename(t, filepath.Join(root, ".git"), root+"-old-git")
					mustRename(t, other, filepath.Join(root, ".git"))
				}
				return nil
			}}
		}, nil, []string{changed}, false},
		{"git dir denied", gitTree, func(_ *testing.T, ws *Workspace, _, _ string) {
			ws.screenSeams = &screenSeams{openErr: answer(".git", fs.ErrPermission)}
		}, nil, []string{withheldNote + `unreadable: ".git")`}, false},
		{"config stat fails", gitTree, func(_ *testing.T, ws *Workspace, _, _ string) {
			ws.screenSeams = &screenSeams{statErr: answer(".git/config", syscall.EIO)}
		}, nil, []string{withheldNote + `unreadable: ".git/config")`}, false},
		{"config gone at stat", gitTree, func(_ *testing.T, ws *Workspace, _, _ string) {
			ws.screenSeams = &screenSeams{statErr: answer(".git/config", fs.ErrNotExist)}
		}, nil, nil, false},
		{"config open denied", gitTree, func(_ *testing.T, ws *Workspace, _, _ string) {
			ws.screenSeams = &screenSeams{openErr: answer(".git/config", fs.ErrPermission)}
		}, nil, []string{withheldNote + `unreadable: ".git/config")`}, false},
		// A config that cannot be opened for a reason other than a change: a
		// Unix socket renamed onto it after its stat, so the real open in
		// openRegular fails (ENXIO on Linux, EOPNOTSUPP on darwin) under any
		// uid and without permission bits.
		{"config becomes a socket", gitTree, func(t *testing.T, ws *Workspace, root, _ string) {
			dir, err := os.MkdirTemp("/tmp", "scr")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			sock := filepath.Join(dir, "s")
			l, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			ws.screenSeams = &screenSeams{openErr: func(rel string) error {
				if rel == ".git/config" {
					mustRename(t, sock, filepath.Join(root, ".git", "config"))
				}
				return nil
			}}
		}, nil, []string{withheldNote + `unreadable: ".git/config")`}, false},
		{"config read fails", gitTree, func(_ *testing.T, ws *Workspace, _, _ string) {
			ws.screenSeams = &screenSeams{readFrom: failRead(".git/config")}
		}, nil, []string{withheldNote + `unreadable: ".git/config")`}, false},
		{"config replaced before open", gitTree, func(t *testing.T, ws *Workspace, root, _ string) {
			other := root + "-other-config"
			if err := os.WriteFile(other, []byte("[core]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ws.screenSeams = &screenSeams{openErr: func(rel string) error {
				if rel == ".git/config" {
					mustRename(t, other, filepath.Join(root, ".git", "config"))
				}
				return nil
			}}
		}, nil, []string{changed}, false},
		{"root swapped at git dir", gitTree, func(t *testing.T, ws *Workspace, root, sib string) {
			ws.screenSeams = &screenSeams{openErr: func(rel string) error {
				if rel != ".git" {
					return nil
				}
				swapRoot(t, root, sib)
				return fs.ErrNotExist // the ordinary error, not the sentinel
			}}
		}, ErrRootReplaced, nil, false},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			ws, root, sib := tolerantFixture(t, r.tree, map[string]string{"a.txt": "x\n"})
			r.setup(t, ws, root, sib)
			s, err := buildOutputScreen(t.Context(), ws)
			if r.wantErr != nil {
				if !errors.Is(err, r.wantErr) || s != nil {
					t.Fatalf("%s: wrong outcome (error: %t, screen: %t)", r.label, err != nil, s != nil)
				}
				secretFree(t, r.label, []string{"pass-0020-abcdef"}, err.Error())
				return
			}
			if err != nil || s == nil {
				t.Fatalf("%s: build failed (error: %t)", r.label, err != nil)
			}
			notes := s.notes()
			if !slices.Equal(notes, r.notes) {
				t.Errorf("%s: notes differ (%d notes, want %d)", r.label, len(notes), len(r.notes))
			}
			secretFree(t, r.label, []string{"pass-0020-abcdef"}, notes...)
			want := "pass-0020-abcdef\n"
			if r.keyed {
				want = tM + "\n"
			} else if len(r.notes) == 1 && strings.HasPrefix(r.notes[0], withheldNote) {
				want = ""
			}
			if got := screenThrough(s, "pass-0020-abcdef\n"); got != want {
				t.Fatalf("%s: output differs (len %d, want %d)", r.label, len(got), len(want))
			}
		})
	}
}

// The screen's walk reads what the scope guard hides; the strict walk still
// obeys the guard.
func TestBuildOutputScreenIgnoresGuard(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	ws, _, _ := tolerantFixture(t, map[string]string{"secrets/.env": "TOKEN=guard-0001-abcdef\n", "a.txt": "x\n"}, nil)
	ws.SetScopeGuard(func(rel string, _ bool) error {
		if rel == "secrets" || strings.HasPrefix(rel, "secrets/") {
			return errors.New("denied")
		}
		return nil
	})
	s := mustBuild(t, "guard", ws)
	if got := screenThrough(s, "got guard-0001-abcdef\n"); got != "got "+tM+"\n" {
		t.Fatalf("guard: key not collected (len %d)", len(got))
	}
	var visits []string
	if err := ws.walk(t.Context(), func(rel string, _ fs.DirEntry) error {
		visits = append(visits, rel)
		return nil
	}); err != nil || !slices.Equal(visits, []string{"a.txt"}) {
		t.Fatalf("guard: strict walk visits differ (%d, error: %t)", len(visits), err != nil)
	}
}
