//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This is the required real-application terminal lifecycle gate (spec 13.2).
// The fake termOps suite proves exact call order; only this proves that a real
// termios, put into raw mode by the real x/term and restored by production
// teardown, comes back byte-identical on every exit path the REPL has.
//
// Calling MakeRaw and Restore directly on a PTY would only retest x/term. Every
// case here therefore runs the whole production stack: newInput selects the
// source, replControl owns interrupt policy, withLineSource owns Close, and
// runREPL is the loop.
//
// Linux only, by build tag. macOS needs posix_openpt, which x/sys does not
// wrap; the development host keeps the fake call-order coverage and this runs
// in Docker and GitHub CI.

func openPTY(t *testing.T) *ptyPair {
	t.Helper()
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	// Unlock before opening the slave; the pair is locked at creation.
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(mfd)
		t.Fatalf("TIOCSPTLCK: %v", err)
	}
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(mfd)
		t.Fatalf("TIOCGPTN: %v", err)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", n)
	sfd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = unix.Close(mfd)
		t.Fatalf("open %s: %v", slavePath, err)
	}
	// A fresh PTY has no window size. x/term would then wrap after every
	// column-zero character and the prompt would arrive as one rune per line,
	// which no real terminal does. Give it ordinary dimensions so rendering is
	// representative.
	if err := unix.IoctlSetWinsize(sfd, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		_ = unix.Close(mfd)
		_ = unix.Close(sfd)
		t.Fatalf("TIOCSWINSZ: %v", err)
	}
	p := &ptyPair{
		master: os.NewFile(uintptr(mfd), "/dev/ptmx"),
		slave:  os.NewFile(uintptr(sfd), slavePath),
	}
	t.Cleanup(p.close)
	return p
}

// termios reads the slave's current terminal settings. The struct is
// comparable, so equality is a byte-for-byte check of every flag and control
// character -- which is the actual claim: not "restored approximately".
func (p *ptyPair) termios(t *testing.T) unix.Termios {
	t.Helper()
	st, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("TCGETS: %v", err)
	}
	return *st
}

// ptyREPL runs one full REPL over the pair and returns a channel carrying the
// loop's error and any recovered panic value. onInterrupt is the policy hook;
// nil installs the real replControl behavior.
type ptyResult struct {
	err       error
	recovered any
}

func startPTYREPL(t *testing.T, p *ptyPair, onInterrupt func(func())) (<-chan ptyResult, *lockedBuffer) {
	t.Helper()
	root := t.TempDir()
	sess := newTestSession(t, &scriptCaller{}, root)

	seen := drainMaster(p)
	errOut := &lockedBuffer{}

	replCtx, cancelREPL := context.WithCancel(context.Background())
	t.Cleanup(cancelREPL)
	interrupts := make(chan struct{}, 1)
	ctrl := newReplControl(p.slave, errOut, interrupts, cancelREPL)
	sess.control = ctrl

	hook := ctrl.interrupt
	if onInterrupt != nil {
		hook = func() { onInterrupt(ctrl.interrupt) }
	}

	// Real descriptors, real termOps: MakeRaw and Restore act on the PTY.
	src := newInput(inputConfig{
		Stdin: p.slave, Stdout: p.slave, Stderr: errOut,
		Getenv:      func(string) string { return "" },
		Root:        root,
		OnInterrupt: hook,
	})
	if _, isEditor := src.(*editorSource); !isEditor {
		t.Fatalf("newInput selected %T over a real PTY, want the editor", src)
	}
	ctrl.setIdleDisplay(src.IdleDisplay)

	done := make(chan ptyResult, 1)
	go func() {
		var res ptyResult
		defer func() {
			res.recovered = recover()
			done <- res
		}()
		res.err = withLineSource(src, func(s lineSource) error {
			return runREPL(replCtx, s, p.slave, interrupts, sess)
		})
	}()
	return done, seen
}

// awaitPTY waits for the loop to finish. On timeout it closes both ends first,
// so a blocked read(2) cannot leave the goroutine parked and the failure is
// reported instead of hanging CI.
func awaitPTY(t *testing.T, p *ptyPair, done <-chan ptyResult) ptyResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(10 * time.Second):
		p.close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("the REPL did not exit within the timeout")
		return ptyResult{}
	}
}

// awaitPrompt blocks until the prompt is on screen, which happens only after
// MakeRaw has succeeded and ReadLine is inside the raw window.
//
// This barrier is required, not cosmetic. A byte written while the line
// discipline is still canonical is interpreted by the kernel rather than
// delivered: 0x04 becomes a VEOF marker that the switch to non-canonical mode
// discards, and 0x03 becomes a signal character for a foreground process group
// that does not exist. Either way the byte never reaches the editor and the
// read blocks forever.
func awaitPrompt(t *testing.T, p *ptyPair, seen *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(seen.String(), promptText) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	p.close()
	t.Fatalf("the prompt never reached the terminal; master saw %q", seen.String())
}

func writeMaster(t *testing.T, p *ptyPair, s string) {
	t.Helper()
	if _, err := p.master.WriteString(s); err != nil {
		t.Fatalf("write %q to the pty master: %v", s, err)
	}
}

func TestPTYRestoresTermiosOnEveryExit(t *testing.T) {
	// One case per way the REPL can end. Each asserts the same thing the user
	// actually cares about: the shell they return to is not left in raw mode.
	t.Run("slash exit", func(t *testing.T) {
		p := openPTY(t)
		before := p.termios(t)
		done, seen := startPTYREPL(t, p, nil)
		awaitPrompt(t, p, seen)
		writeMaster(t, p, "/exit\r")
		if res := awaitPTY(t, p, done); res.err != nil || res.recovered != nil {
			t.Fatalf("/exit = err %v panic %v, want a clean exit", res.err, res.recovered)
		}
		if after := p.termios(t); after != before {
			t.Fatalf("termios not restored after /exit:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("ctrl-d on an empty line", func(t *testing.T) {
		p := openPTY(t)
		before := p.termios(t)
		done, seen := startPTYREPL(t, p, nil)
		awaitPrompt(t, p, seen)
		writeMaster(t, p, "\x04")
		if res := awaitPTY(t, p, done); res.err != nil || res.recovered != nil {
			t.Fatalf("Ctrl-D = err %v panic %v, want a clean exit", res.err, res.recovered)
		}
		if after := p.termios(t); after != before {
			t.Fatalf("termios not restored after Ctrl-D:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("idle double ctrl-c", func(t *testing.T) {
		p := openPTY(t)
		before := p.termios(t)
		done, seen := startPTYREPL(t, p, nil)
		awaitPrompt(t, p, seen)

		// The presses must be two distinct reads. The interrupt cycle discards
		// whatever the filter retained past the first 0x03, so a single write
		// of both bytes would drop the second. Waiting for the hint is the
		// barrier that proves the first press was fully processed.
		writeMaster(t, p, "\x03")
		waitFor(t, func() bool { return strings.Contains(seen.String(), ctrlCHint) })
		writeMaster(t, p, "\x03")

		if res := awaitPTY(t, p, done); res.err != nil || res.recovered != nil {
			t.Fatalf("double Ctrl-C = err %v panic %v, want a clean exit", res.err, res.recovered)
		}
		if got := strings.Count(seen.String(), ctrlCHint); got != 1 {
			t.Fatalf("hint printed %d times over a real terminal, want exactly 1", got)
		}
		if after := p.termios(t); after != before {
			t.Fatalf("termios not restored after a double Ctrl-C:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("panic in the interrupt hook", func(t *testing.T) {
		// The raw-window defer must restore termios while the stack unwinds.
		// A panic that escaped with the terminal still raw would leave the
		// user's shell unusable, which no error path can repair afterwards.
		p := openPTY(t)
		before := p.termios(t)
		done, seen := startPTYREPL(t, p, func(func()) {
			panic("interrupt hook exploded")
		})
		awaitPrompt(t, p, seen)
		writeMaster(t, p, "\x03")

		res := awaitPTY(t, p, done)
		if res.recovered == nil {
			t.Fatal("the panic did not propagate out of the REPL; this case proves nothing without it")
		}
		if after := p.termios(t); after != before {
			t.Fatalf("termios not restored after a panic:\nbefore %+v\nafter  %+v", before, after)
		}
	})
}

func TestPTYRawModeIsActuallyEntered(t *testing.T) {
	// Guards the other three cases from passing vacuously. If the editor never
	// put the terminal into raw mode, every "restored" assertion above would
	// hold trivially, so this pins that termios genuinely changes mid-read:
	// ECHO and ICANON off, ISIG off (which is why 0x03 arrives as a byte).
	p := openPTY(t)
	before := p.termios(t)

	root := t.TempDir()
	seen := drainMaster(p)
	errOut := &lockedBuffer{}
	src := newInput(inputConfig{
		Stdin: p.slave, Stdout: p.slave, Stderr: errOut,
		Getenv: func(string) string { return "" },
		Root:   root,
	})
	read := make(chan struct{})
	// Close is owned by the mode boundary and must not overlap a live read; on
	// a t.Fatal path below the reader is still parked inside ReadLine, so this
	// cleanup unblocks it and joins before closing. Registering a bare
	// src.Close() here is a genuine -race flake, not a theoretical one.
	t.Cleanup(func() {
		p.close() // idempotent, and unblocks a parked ReadLine
		select {
		case <-read:
		case <-time.After(5 * time.Second):
			t.Error("the read goroutine never returned; closing the source would race it")
			return
		}
		_ = src.Close()
	})

	go func() {
		defer close(read)
		_, _, _ = src.ReadGoal(context.Background(), promptText)
	}()

	// The prompt on screen means the raw window is open and ReadLine is
	// blocked inside it.
	awaitPrompt(t, p, seen)
	during := p.termios(t)
	if during.Lflag&unix.ECHO != 0 || during.Lflag&unix.ICANON != 0 || during.Lflag&unix.ISIG != 0 {
		t.Fatalf("terminal is not in raw mode during a read: Lflag = %#x", during.Lflag)
	}
	if during == before {
		t.Fatal("termios unchanged during a read; the restore assertions would pass vacuously")
	}

	writeMaster(t, p, "\r")
	select {
	case <-read:
	case <-time.After(10 * time.Second):
		p.close()
		t.Fatal("the read did not return")
	}
	if after := p.termios(t); after != before {
		t.Fatalf("termios not restored after one read:\nbefore %+v\nafter  %+v", before, after)
	}
}

// Compile-time assertion: the PTY helpers use the same io.Writer the editor
// writes through, so a future signature change breaks here rather than at
// runtime in CI only.
var _ io.Writer = (*lockedBuffer)(nil)

// sgr matches a Select Graphic Rendition sequence (color or weight), not the
// editor's cursor and bracketed-paste controls, which also start with ESC.
var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

// noColorCases is the -no-color matrix: painted by default (the positive
// control proving the terminal path colors at all), and plain with the flag.
var noColorCases = []struct {
	name    string
	flags   []string
	painted bool
}{
	{"default", nil, true},
	{"-no-color", []string{"-no-color"}, false},
}

// checkPainted asserts the model-server section in text is painted (its bold
// title) or carries no SGR at all.
func checkPainted(t *testing.T, text string, painted bool) {
	t.Helper()
	if !strings.Contains(text, "model servers") {
		t.Fatalf("section not drawn on the terminal: %q", text)
	}
	if painted && !strings.Contains(text, "\x1b[1mmodel servers\x1b[0m") {
		t.Errorf("section not painted without -no-color: %q", text)
	}
	if m := sgr.FindString(text); !painted && m != "" {
		t.Errorf("SGR %q with -no-color: %q", m, text)
	}
}

// flushPTY writes a marker to the slave and waits for the master to read it,
// so everything written before the call is in seen; it returns the text
// before the marker.
func flushPTY(t *testing.T, p *ptyPair, seen *lockedBuffer) string {
	t.Helper()
	const end = "END-OF-PTY-OUTPUT"
	if _, err := p.slave.WriteString(end); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := seen.String()
		if i := strings.Index(s, end); i >= 0 {
			return s[:i]
		}
		if time.Now().After(deadline) {
			p.close()
			t.Fatalf("the marker never reached the master: %q", s)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestIndexNoColorHonored runs golem index with a real terminal as stderr:
// the model-server section is colored by default, and -no-color leaves no
// escape sequence. The remote embedding is refused, so nothing is sent.
func TestIndexNoColorHonored(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	configPath, _ := subcommandHarness(t, "remote")
	for _, tc := range noColorCases {
		t.Run(tc.name, func(t *testing.T) {
			p := openPTY(t)
			seen := drainMaster(p)
			args := append([]string{"-config", configPath, "-root", t.TempDir()}, tc.flags...)
			if err := runIndex(context.Background(), args, io.Discard, p.slave); err == nil {
				t.Fatal("index with an uncovered remote embedding succeeded")
			}
			got := flushPTY(t, p, seen)
			checkPainted(t, got, tc.painted)
			if has := strings.Contains(got, "\x1b"); has != tc.painted {
				t.Errorf("escape sequences present = %v, want %v: %q", has, tc.painted, got)
			}
		})
	}
}

// TestStartupNoColorHonored draws the startup consent section on a terminal
// stderr: painted by default, no SGR with -no-color. -grounding makes the
// remote reachable in one-shot, so the run is refused before any request.
func TestStartupNoColorHonored(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	configPath, root, requests := admissionHarness(t, "https://opencode.invalid/zen/go")
	for _, tc := range noColorCases {
		t.Run(tc.name, func(t *testing.T) {
			p := openPTY(t)
			seen := drainMaster(p)
			stdin, stdout, _ := runTestFiles(t)
			args := append(append(admissionArgs(configPath, root), "-grounding"), tc.flags...)
			if err := run(args, stdin, stdout, p.slave); err == nil {
				t.Fatal("startup with an uncovered remote was admitted")
			}
			checkPainted(t, flushPTY(t, p, seen), tc.painted)
		})
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("refused startups sent %d requests, want 0", got)
	}
}

// replArgs runs the REPL with nothing outside the test's control.
func replArgs(configPath, root string) []string {
	return []string{"-config", configPath, "-root", root,
		"-no-probe", "-no-cap-probe", "-no-memory", "-no-rag", "-no-project-context",
		"-no-git-context", "-no-auto-index", "-no-session"}
}

// startRun runs golem with the pair as stdin and stdout (so the line editor
// is selected) and stderr as given, returning run's result.
func startRun(p *ptyPair, args []string, stderr *os.File) <-chan error {
	done := make(chan error, 1)
	go func() { done <- run(args, p.slave, p.slave, stderr) }()
	return done
}

// awaitRun polls cond while run is live: it fails at once if run returns
// first, and closes the pair on timeout so nothing stays blocked.
func awaitRun(t *testing.T, p *ptyPair, seen *lockedBuffer, done <-chan error, stderr *os.File, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		select {
		case err := <-done:
			t.Fatalf("golem exited before %s: %v\nstderr:\n%s\nterminal: %q", what, err, readRunTestFile(t, stderr), seen.String())
		default:
		}
		if time.Now().After(deadline) {
			p.close()
			t.Fatalf("timed out waiting for %s\nstderr:\n%s\nterminal: %q", what, readRunTestFile(t, stderr), seen.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// exitRun types /exit (the caller has seen a prompt, so the raw window is
// open) and waits for a clean return.
func exitRun(t *testing.T, p *ptyPair, done <-chan error, stderr *os.File) {
	t.Helper()
	writeMaster(t, p, "/exit\r")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
		}
	case <-time.After(10 * time.Second):
		p.close()
		t.Fatal("golem did not exit after /exit")
	}
}

// TestGrantsNoColorHonored types /grants into the REPL on a terminal stdout:
// the listing is painted by default and carries no SGR with -no-color. The
// config is local-only, so startup asks nothing.
func TestGrantsNoColorHonored(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	local := newModelBackend(t, "local", "agent-model")
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {"local": {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"}},
  "models": {"agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`, local.url)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range noColorCases {
		t.Run(tc.name, func(t *testing.T) {
			p := openPTY(t)
			seen := drainMaster(p)
			_, _, stderr := runTestFiles(t)
			done := startRun(p, append(replArgs(configPath, t.TempDir()), tc.flags...), stderr)
			awaitPrompt(t, p, seen)
			writeMaster(t, p, "/grants\r")
			// The editor draws one prompt per read: the second follows the
			// listing and opens the raw window /exit needs.
			awaitRun(t, p, seen, done, stderr, "the prompt after /grants", func() bool {
				return strings.Count(seen.String(), promptText) >= 2
			})
			exitRun(t, p, done, stderr)
			out := seen.String()
			i := strings.Index(out, "session grants:")
			if i < 0 {
				t.Fatalf("/grants printed nothing: %q", out)
			}
			listing := out[i:]
			if j := strings.Index(listing, promptText); j >= 0 {
				listing = listing[:j]
			}
			checkPainted(t, listing, tc.painted)
		})
	}
}

// TestModelSetSectionPaintedOnTerminal runs /model set to an uncovered remote
// with the REPL on a terminal and stderr in a file: the section is drawn on the
// terminal, where the question is asked, with the terminal's style (painted by
// default, no SGR with -no-color), and none of it reaches stderr. The answer is
// no, so nothing is sent.
func TestModelSetSectionPaintedOnTerminal(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	fx := newModelSwitchFixture(t, remoteModelURL)
	const question = "Send data to cloud (remote) during this session? [y/N] "
	for _, tc := range noColorCases {
		t.Run(tc.name, func(t *testing.T) {
			p := openPTY(t)
			seen := drainMaster(p)
			_, _, stderr := runTestFiles(t)
			done := startRun(p, append(replArgs(fx.configPath, t.TempDir()), tc.flags...), stderr)
			awaitPrompt(t, p, seen)
			writeMaster(t, p, "/model set cloudrole\r")
			// The question is the answer read's prompt, drawn once its raw
			// window is open: the barrier before typing the answer.
			awaitRun(t, p, seen, done, stderr, "the consent question", func() bool {
				return strings.Contains(seen.String(), question)
			})
			writeMaster(t, p, "n\r")
			awaitRun(t, p, seen, done, stderr, "the prompt after the refusal", func() bool {
				return strings.Count(seen.String(), promptText) >= 2
			})
			exitRun(t, p, done, stderr)

			out := seen.String()
			s, q := strings.Index(out, "model servers"), strings.Index(out, question)
			if s < 0 || s > q {
				t.Fatalf("section not on the terminal before the question: %q", out)
			}
			section := out[strings.LastIndex(out[:s], "\n")+1 : q]
			checkPainted(t, section, tc.painted)
			if !strings.Contains(section, "model-switch.invalid") {
				t.Errorf("the remote's row is not in the terminal section: %q", section)
			}
			errText := readRunTestFile(t, stderr)
			for _, leak := range []string{"model-switch.invalid", "needs your OK", "\nRemote\n", question} {
				if strings.Contains(errText, leak) {
					t.Errorf("stderr carries %q of the /model set section:\n%s", leak, errText)
				}
			}
		})
	}
}

// TestModelSetSectionPipedStdoutTerminalStderr is the reverse orientation: stderr is
// the terminal and stdout a pipe, so the scanner reads goals and prints its
// prompts on stdout. The startup section is painted on the terminal by
// default (the positive control: stderr is the styled path), while the /model
// set section and its question are drawn on stdout with no SGR, and no remote
// row reaches the terminal: neither the writer nor its style is inherited
// from startup. The answer is no, so nothing is sent.
func TestModelSetSectionPipedStdoutTerminalStderr(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	fx := newModelSwitchFixture(t, remoteModelURL)
	const question = "Send data to cloud (remote) during this session? [y/N] "
	const cloudRow = "\n  cloud    https://model-switch.invalid/v1  needs your OK\n"
	for _, tc := range noColorCases {
		t.Run(tc.name, func(t *testing.T) {
			p := openPTY(t)
			seen := drainMaster(p)
			inR, inW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			outR, outW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = inR.Close(); _ = inW.Close(); _ = outR.Close(); _ = outW.Close() })
			stdout := &lockedBuffer{}
			copied := make(chan struct{})
			go func() { _, _ = io.Copy(stdout, outR); close(copied) }()
			done := make(chan error, 1)
			go func() { done <- run(append(replArgs(fx.configPath, t.TempDir()), tc.flags...), inR, outW, p.slave) }()

			// The scanner prints each prompt on stdout before it reads: the
			// barrier before each line typed.
			wait := func(what string, cond func() bool) {
				t.Helper()
				deadline := time.Now().Add(20 * time.Second)
				for !cond() {
					select {
					case err := <-done:
						t.Fatalf("golem exited before %s: %v\nstdout: %q\nterminal: %q", what, err, stdout.String(), seen.String())
					default:
					}
					if time.Now().After(deadline) {
						p.close()
						_ = inW.Close()
						t.Fatalf("timed out waiting for %s\nstdout: %q\nterminal: %q", what, stdout.String(), seen.String())
					}
					time.Sleep(time.Millisecond)
				}
			}
			send := func(line string) {
				t.Helper()
				if _, err := inW.WriteString(line); err != nil {
					t.Fatal(err)
				}
			}
			wait("the first prompt", func() bool { return strings.Contains(stdout.String(), promptText) })
			send("/model set cloudrole\n")
			wait("the consent question", func() bool { return strings.Contains(stdout.String(), question) })
			send("n\n")
			wait("the prompt after the refusal", func() bool { return strings.Count(stdout.String(), promptText) >= 2 })
			send("/exit\n")
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("run = %v\nstdout:\n%s", err, stdout.String())
				}
			case <-time.After(10 * time.Second):
				p.close()
				_ = inW.Close()
				t.Fatal("golem did not exit after /exit")
			}
			_ = outW.Close()
			<-copied

			out := stdout.String()
			s, q := strings.Index(out, "model servers"), strings.Index(out, question)
			if s < 0 || s > q {
				t.Fatalf("section not on stdout before the question:\n%s", out)
			}
			section := out[strings.LastIndex(out[:s], "\n")+1 : q+len(question)]
			if m := sgr.FindString(section); m != "" {
				t.Errorf("SGR %q in the stdout section or question: %q", m, section)
			}
			if !strings.Contains(section, cloudRow) {
				t.Errorf("the remote's row is not in the stdout section: %q", section)
			}
			// The startup section names only the local server, so the whole
			// terminal text, startup included, must carry no remote row text.
			term := flushPTY(t, p, seen)
			checkPainted(t, term, tc.painted)
			leaks := []string{"cloud", "model-switch.invalid", "Remote", question}
			for _, st := range []remoteState{remoteNeedsOK, remoteAllowedByFlag, remoteAllowedEarlier, remoteNotAllowed, remoteRefused} {
				leaks = append(leaks, st.text())
			}
			for _, leak := range leaks {
				if strings.Contains(term, leak) {
					t.Errorf("the terminal (stderr) carries %q of the /model set section:\n%q", leak, term)
				}
			}
		})
	}
}

// TestStartupConsentTypeahead types "y\nhello\n" before golem reads anything:
// the startup consent answers yes, the typeahead after it survives into the
// line editor, and the first goal is "hello". It does not prove byte-at-a-time
// reading: in canonical mode one read(2) returns at most a line, so a single
// buffered read of the answer would pass too.
func TestStartupConsentTypeahead(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	local := newModelBackend(t, "local", "agent-model")
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "local": {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"},
    "cloud": {"base_url": "https://typeahead.invalid/v1", "api_format": "openai-compat", "timeout": "5s"}
  },
  "models": {
    "agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"]},
    "summ": {"name": "summ-model", "provider": "cloud", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream"]}
  },
  "defaults": {"agent": "agent", "summarize": "summ"}
}`, local.url)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	p := openPTY(t)
	seen := drainMaster(p)
	_, _, stderr := runTestFiles(t)

	// Typed before run starts: the line discipline holds it for whoever
	// reads first.
	writeMaster(t, p, "y\nhello\n")
	done := startRun(p, replArgs(configPath, t.TempDir()), stderr)
	awaitRun(t, p, seen, done, stderr, "the first goal", func() bool { return local.chats.Load() >= 1 })
	// The editor draws one prompt per read, so a second one on screen means
	// the raw window is open again (the awaitPrompt barrier) and /exit
	// reaches it.
	awaitRun(t, p, seen, done, stderr, "the next prompt", func() bool {
		return strings.Count(seen.String(), promptText) >= 2
	})
	exitRun(t, p, done, stderr)

	if bodies := local.chatBodies(); len(bodies) != 1 || !strings.Contains(bodies[0], "hello") {
		t.Errorf("chat requests = %q, want exactly one carrying the goal \"hello\"", bodies)
	}
	const question = "Send data to cloud (remote) during this session? [y/N] "
	if errOut := readRunTestFile(t, stderr); strings.Count(errOut, question) != 1 {
		t.Errorf("startup consent asked %d times, want once:\n%s", strings.Count(errOut, question), errOut)
	}
}
