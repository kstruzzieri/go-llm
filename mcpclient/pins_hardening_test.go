package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinFilesystemRefusal(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "oversize", "disappeared", "replaced", "lock-symlink", "lock-directory", "directory-symlink"} {
		t.Run(kind, func(t *testing.T) {
			s := pinStoreForTest(t)
			ctx := context.Background()
			a := pinCatalog(t, "fs", "A")
			if _, _, err := s.admit(ctx, "fs", a, false); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.dir, fsKey+".json")
			original := readPinBytes(t, s)
			switch kind {
			case "symlink", "lock-symlink":
				if kind == "lock-symlink" {
					path = filepath.Join(s.dir, fsKey+".lock")
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
					t.Skip(err)
				}
			case "directory", "lock-directory":
				if kind == "lock-directory" {
					path = filepath.Join(s.dir, fsKey+".lock")
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(path, bytes.Repeat([]byte(" "), 16*1024*1024+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "disappeared", "replaced":
				s.ops.afterLstat = func(name string) {
					if name != fsKey+".json" {
						return
					}
					if kind == "replaced" {
						// Keep both inodes alive before the swap: remove+write can
						// recycle the original inode on Linux.
						if err := os.WriteFile(path+".replacement", original, 0600); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if kind == "replaced" {
						if err := os.Rename(path+".replacement", path); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "directory-symlink":
				target := s.dir + "-moved"
				if err := os.Rename(s.dir, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, s.dir); err != nil {
					t.Skip(err)
				}
			}
			if _, created, err := s.admit(ctx, "fs", a, false); err == nil || created {
				t.Fatalf("unsafe %s admitted: %v %v", kind, created, err)
			}
		})
	}
}

func TestPinPublicationFaults(t *testing.T) {
	for _, stage := range []string{"write", "short-write", "sync", "rename", "dir-sync", "cancel", "temp-replaced"} {
		t.Run(stage, func(t *testing.T) {
			s := pinStoreForTest(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := pinCatalog(t, "fs", "A")
			b := pinCatalog(t, "fs", "B")
			if _, _, err := s.admit(ctx, "fs", a, false); err != nil {
				t.Fatal(err)
			}
			raw := readPinBytes(t, s)
			_, rev, err := s.capturePin(ctx, "fs")
			if err != nil {
				t.Fatal(err)
			}
			fault := errors.New("injected " + stage)
			switch stage {
			case "write":
				s.ops.write = func(*os.File, []byte) (int, error) { return 0, fault }
			case "short-write":
				s.ops.write = func(*os.File, []byte) (int, error) { return 1, nil }
			case "sync":
				s.ops.syncFile = func(*os.File) error { return fault }
			case "rename":
				s.ops.rename = func(*os.Root, string, string) error { return fault }
			case "dir-sync":
				realRename := s.ops.rename
				renamed := false
				s.ops.rename = func(r *os.Root, a, b string) error { err := realRename(r, a, b); renamed = err == nil; return err }
				s.ops.syncDir = func(r *os.Root) error {
					if renamed {
						return fault
					}
					return syncPinDirectory(r)
				}
			case "cancel":
				s.ops.beforePublish = cancel
			case "temp-replaced":
				s.ops.beforePublish = func() {
					paths, err := filepath.Glob(filepath.Join(s.dir, ".*.tmp-*"))
					if err != nil || len(paths) != 1 {
						t.Fatalf("temp: %v %v", paths, err)
					}
					// Pre-create the replacement while the original inode is live.
					replacement := filepath.Join(s.dir, "replacement")
					if err = os.WriteFile(replacement, []byte("corrupt replacement"), 0600); err != nil {
						t.Fatal(err)
					}
					if err = os.Remove(paths[0]); err != nil {
						t.Fatal(err)
					}
					if err = os.Rename(replacement, paths[0]); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = s.replacePin(ctx, "fs", rev, b)
			if err == nil {
				t.Fatal("fault admitted candidate")
			}
			got := readPinBytes(t, s)
			if stage == "dir-sync" {
				if !errors.Is(err, errPinDurability) || bytes.Equal(raw, got) {
					t.Fatalf("post publish durability: %v", err)
				}
				s.ops = defaultPinFileOps()
				prior, _, err := s.admit(context.Background(), "fs", b, true)
				if err != nil || prior.digest() != b.digest() {
					t.Fatalf("durability retry: %v", err)
				}
			} else if !bytes.Equal(raw, got) {
				t.Fatalf("%s changed old pin", stage)
			}
			temps, err := filepath.Glob(filepath.Join(s.dir, ".*.tmp-*"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("temporary files remain: %v %v", temps, err)
			}
		})
	}
}

func TestPinLeaseAliasAndInstanceIsolation(t *testing.T) {
	for _, two := range []bool{false, true} {
		t.Run(fmt.Sprint(two), func(t *testing.T) {
			s := pinStoreForTest(t)
			other := s
			if two {
				var err error
				other, err = newPinStore(s.workspace, s.base)
				if err != nil {
					t.Fatal(err)
				}
			}
			root, err := s.openRoot(false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			lease, err := s.acquireLease(root, fsKey+".lock")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lease.Close() }()
			// Same literal alias locks independently opened file handles, while its
			// uppercase counterpart makes progress on the actual filesystem.
			if _, _, err = other.admit(context.Background(), "FS", pinCatalog(t, "FS", "A"), false); err != nil {
				t.Fatal(err)
			}
			blocked := make(chan struct{})
			release := make(chan struct{})
			other.ops.wait = func(ctx context.Context) error {
				close(blocked)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := other.admit(context.Background(), "fs", pinCatalog(t, "fs", "A"), false)
				done <- err
			}()
			<-blocked
			if err = lease.Close(); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(s.dir, fsKey+".lock")); err != nil {
				t.Fatalf("unlock removed lock path: %v", err)
			}
		})
	}
}

func TestPinLeaseCancellationAndContention(t *testing.T) {
	for _, cancelAfterLock := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelAfterLock), func(t *testing.T) {
			s := pinStoreForTest(t)
			root, err := s.openRoot(false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			lease, err := s.acquireLease(root, fsKey+".lock")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lease.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.ops.wait = func(context.Context) error {
				if cancelAfterLock {
					if err := lease.Close(); err != nil {
						t.Fatal(err)
					}
					cancel()
					return nil
				}
				return context.DeadlineExceeded
			}
			_, created, err := s.admit(ctx, "fs", pinCatalog(t, "fs", "A"), false)
			if err == nil || created {
				t.Fatalf("contention/cancellation wrote: %v %v", created, err)
			}
			// The operator-visible classification must follow the cause: the
			// acquisition budget's own expiry is contention, never cancellation.
			reason := admissionFailure("fs", "pin_unavailable", err).Reason
			if cancelAfterLock && (!errors.Is(err, context.Canceled) || reason != "canceled") {
				t.Fatalf("cancel after acquisition: %v (%s)", err, reason)
			}
			if !cancelAfterLock && (!errors.Is(err, errPinContention) || errors.Is(err, context.DeadlineExceeded) || reason != "pin_contention") {
				t.Fatalf("not contention: %v (%s)", err, reason)
			}
			if _, err = os.Stat(filepath.Join(s.dir, fsKey+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected pin: %v", err)
			}
		})
	}
}

func TestPinLeaseCallerCancellationDuringWait(t *testing.T) {
	s := pinStoreForTest(t)
	root, err := s.openRoot(false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lease, err := s.acquireLease(root, fsKey+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ops.wait = func(budget context.Context) error {
		cancel()
		return budget.Err()
	}
	_, created, err := s.admit(ctx, "fs", pinCatalog(t, "fs", "A"), false)
	if err == nil || created {
		t.Fatalf("cancelled wait wrote: %v %v", created, err)
	}
	if reason := admissionFailure("fs", "pin_unavailable", err).Reason; !errors.Is(err, errPinContention) || !errors.Is(err, context.Canceled) || reason != "canceled" {
		t.Fatalf("caller cancellation during contention: %v (%s)", err, reason)
	}
}

func TestPinLeaseAcquiredAfterBudgetLapseProceeds(t *testing.T) {
	s := pinStoreForTest(t)
	root, err := s.openRoot(false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lease, err := s.acquireLease(root, fsKey+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	// Release the competing lease only once the acquisition budget has lapsed
	// (a real wait): the retry then acquires with an expired budget and a live
	// caller context, and a held lease must not be reported as a failure.
	s.ops.wait = func(budget context.Context) error {
		<-budget.Done()
		return lease.Close()
	}
	_, created, err := s.admit(context.Background(), "fs", pinCatalog(t, "fs", "A"), false)
	if err != nil || !created {
		t.Fatalf("held lease after budget lapse: created=%v err=%v", created, err)
	}
}

func TestPinCompetingFirstPins(t *testing.T) {
	s := pinStoreForTest(t)
	other, err := newPinStore(s.workspace, s.base)
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{})
	publish := make(chan struct{})
	waiter := make(chan struct{})
	retry := make(chan struct{})
	s.ops.beforePublish = func() { close(reached); <-publish }
	other.ops.wait = func(ctx context.Context) error {
		close(waiter)
		select {
		case <-retry:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a, b := pinCatalog(t, "fs", "A"), pinCatalog(t, "fs", "B")
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, _, err := s.admit(context.Background(), "fs", a, false); first <- err }()
	<-reached
	go func() { _, _, err := other.admit(context.Background(), "fs", b, false); second <- err }()
	<-waiter
	close(publish)
	if err = <-first; err != nil {
		t.Fatal(err)
	}
	close(retry)
	if err = <-second; !errors.Is(err, errPinMismatch) {
		t.Fatalf("competing first pin: %v", err)
	}
	c, _, err := s.capturePin(context.Background(), "fs")
	if err != nil || c.digest() != a.digest() {
		t.Fatalf("winner changed: %v", err)
	}
}

func TestPinCompetingApprovals(t *testing.T) {
	s := pinStoreForTest(t)
	other, err := newPinStore(s.workspace, s.base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := pinCatalog(t, "fs", "A")
	b := pinCatalog(t, "fs", "B")
	if _, _, err = s.admit(ctx, "fs", a, false); err != nil {
		t.Fatal(err)
	}
	_, r1, err := s.capturePin(ctx, "fs")
	if err != nil {
		t.Fatal(err)
	}
	_, r2, err := other.capturePin(ctx, "fs")
	if err != nil {
		t.Fatal(err)
	}
	barrier := make(chan struct{})
	result := make(chan error, 2)
	for _, v := range []struct {
		s *PinStore
		r pinRevision
	}{{s, r1}, {other, r2}} {
		go func() { <-barrier; result <- v.s.replacePin(ctx, "fs", v.r, b) }()
	}
	close(barrier)
	e1, e2 := <-result, <-result
	if (e1 == nil) == (e2 == nil) || (!errors.Is(e1, errPinRevisionConflict) && !errors.Is(e2, errPinRevisionConflict)) {
		t.Fatalf("approvals: %v / %v", e1, e2)
	}
}

func TestPinLeaseProcessExit(t *testing.T) {
	if os.Getenv("GOLEM_PIN_LEASE_CHILD") == "1" {
		s, err := newPinStore(os.Getenv("GOLEM_PIN_WORKSPACE"), os.Getenv("GOLEM_PIN_BASE"))
		if err != nil {
			t.Fatal(err)
		}
		root, err := s.openRoot(false)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := s.acquireLease(root, fsKey+".lock")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		// Deliberately exit without closing either handle; the kernel owns cleanup.
		_ = lease
		os.Exit(0)
	}
	s := pinStoreForTest(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestPinLeaseProcessExit$")
	cmd.Env = append(os.Environ(), "GOLEM_PIN_LEASE_CHILD=1", "GOLEM_PIN_WORKSPACE="+s.workspace, "GOLEM_PIN_BASE="+s.base)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "locked" {
		t.Fatalf("child: %s %s", scanner.Text(), stderr.String())
	}
	root, err := s.openRoot(false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if lease, err := s.acquireLease(root, fsKey+".lock"); !errors.Is(err, errPinContention) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("process lease not held: %v", err)
	}
	if err = input.Close(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatalf("child exit: %v %s", err, stderr.String())
	}
	lease, err := s.acquireLease(root, fsKey+".lock")
	if err != nil {
		t.Fatalf("exit retained lease: %v", err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPinDirectoryDurabilityRetry(t *testing.T) {
	s := pinStoreForTest(t)
	parent := filepath.Base(filepath.Dir(s.dir))
	fault := errors.New("parent sync failed")
	s.ops.syncDir = func(root *os.Root) error {
		if filepath.Base(root.Name()) == parent {
			return fault
		}
		return syncPinDirectory(root)
	}
	if root, err := s.openRoot(true); err == nil {
		_ = root.Close()
		t.Fatal("constructor retry ignored existing parent entry durability")
	}
	synced := false
	s.ops.syncDir = func(root *os.Root) error {
		if filepath.Base(root.Name()) == parent {
			synced = true
		}
		return syncPinDirectory(root)
	}
	root, err := s.openRoot(true)
	if err != nil {
		t.Fatal(err)
	}
	if err = root.Close(); err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Fatal("retry did not confirm existing namespace parent entry")
	}
}

func TestPinIndependentAliasDiscovery(t *testing.T) {
	s := pinStoreForTest(t)
	ctx := context.Background()
	start := make(chan struct{})
	result := make(chan error, 2)
	for _, alias := range []string{"fs", "FS"} {
		candidate := pinCatalog(t, alias, "A")
		go func() { <-start; _, _, err := s.admit(ctx, alias, candidate, false); result <- err }()
	}
	close(start)
	for range 2 {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	for _, alias := range []string{"fs", "FS"} {
		c, rev, err := s.capturePin(ctx, alias)
		if err != nil || !rev.exists || c.digest() != pinCatalog(t, alias, "A").digest() {
			t.Fatalf("alias %s lost pin: %v", alias, err)
		}
	}
}

func TestPinCancellationBeforeIO(t *testing.T) {
	s := pinStoreForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.capturePin(ctx, "fs"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture: %v", err)
	}
	files, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatal("cancelled operation created filesystem paths")
	}
}

func TestPinFirstPublicationDurability(t *testing.T) {
	s := pinStoreForTest(t)
	candidate := pinCatalog(t, "fs", "A")
	s.ops.syncDir = func(*os.Root) error { return errors.New("directory sync failure") }
	if _, created, err := s.admit(context.Background(), "fs", candidate, false); !errors.Is(err, errPinDurability) || created {
		t.Fatalf("first durability failure: created %v err %v", created, err)
	}
	original := readPinBytes(t, s)
	s.ops = defaultPinFileOps()
	if _, created, err := s.admit(context.Background(), "fs", candidate, false); err != nil || created {
		t.Fatalf("durability retry: %v %v", created, err)
	}
	if !bytes.Equal(original, readPinBytes(t, s)) {
		t.Fatal("durability retry rewrote pin")
	}
}
