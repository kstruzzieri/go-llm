package mcpclient

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func baseStdioIdentity() connectionIdentity {
	return connectionIdentity{workspace: "/ws", alias: "fs", kind: "stdio", envBaseline: "unix-v1",
		env: []string{"inherit:TOKEN"}, launcher: "/bin/l", target: "/bin/t", dir: "/w", argv: []string{"l", "--a"}}
}

func mustDigest(t *testing.T, s *PinStore, c connectionIdentity) *connectionPin {
	t.Helper()
	p, err := s.digestConnection(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConnectionFingerprintStableAcrossStores(t *testing.T) {
	ws, base := t.TempDir(), t.TempDir()
	a, err := newPinStore(ws, base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newPinStore(ws, base)
	if err != nil {
		t.Fatal(err)
	}
	pa, pb := mustDigest(t, a, baseStdioIdentity()), mustDigest(t, b, baseStdioIdentity())
	if pa.fingerprint != pb.fingerprint || pa.keyID != pb.keyID || compareConnectionPins(pa, pb) != nil {
		t.Fatal("same identity under the same user key fingerprinted differently")
	}
	if !fingerprintRE.MatchString(pa.fingerprint) || !hexTagRE.MatchString(pa.keyID) {
		t.Fatalf("pin encodings = (%q, %q)", pa.fingerprint, pa.keyID)
	}
}

func TestConnectionChangeLabels(t *testing.T) {
	s := testPins(t)
	base := mustDigest(t, s, baseStdioIdentity())
	for _, tt := range []struct {
		mutate func(*connectionIdentity)
		want   []string
	}{
		{func(c *connectionIdentity) { c.launcher = "/bin/x" }, []string{"launcher"}},
		{func(c *connectionIdentity) { c.target = "/bin/x" }, []string{"target"}},
		{func(c *connectionIdentity) { c.dir = "/x" }, []string{"dir"}},
		{func(c *connectionIdentity) { c.env = []string{"set:TOKEN"} }, []string{"env"}},
		{func(c *connectionIdentity) { c.envBaseline = "windows-v1" }, []string{"env_baseline"}},
		{func(c *connectionIdentity) { c.argv = []string{"l", "--b"} }, []string{"argv"}},
		{func(c *connectionIdentity) { c.launcher, c.target = "/bin/x", "/bin/y" }, []string{"launcher", "target"}},
		{func(c *connectionIdentity) { c.alias = "other" }, []string{"identity"}},
		{func(c *connectionIdentity) {
			*c = connectionIdentity{workspace: "/ws", alias: "fs", kind: "http", origin: "https://h", endpoint: "/"}
		}, []string{"kind"}},
	} {
		c := baseStdioIdentity()
		tt.mutate(&c)
		if got := compareConnectionPins(base, mustDigest(t, s, c)); !slices.Equal(got, tt.want) {
			t.Errorf("labels = %q, want %q", got, tt.want)
		}
	}
	httpBase := connectionIdentity{workspace: "/ws", alias: "api", kind: "http", origin: "https://h", endpoint: "/mcp?q=1"}
	for _, tt := range []struct {
		origin, endpoint string
		want             []string
	}{
		{"https://g", "/mcp?q=1", []string{"origin"}},
		{"https://h", "/mcp?q=2", []string{"endpoint"}},
	} {
		c := httpBase
		c.origin, c.endpoint = tt.origin, tt.endpoint
		if got := compareConnectionPins(mustDigest(t, s, httpBase), mustDigest(t, s, c)); !slices.Equal(got, tt.want) {
			t.Errorf("http labels = %q, want %q", got, tt.want)
		}
	}
}

func TestConnectionKeyLossAndCorruption(t *testing.T) {
	ws, base := t.TempDir(), t.TempDir()
	first, err := newPinStore(ws, base)
	if err != nil {
		t.Fatal(err)
	}
	before := mustDigest(t, first, baseStdioIdentity())
	keyPath := filepath.Join(base, "golem", "mcp-pins", connectionKeyFile)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	recreated, err := newPinStore(ws, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := compareConnectionPins(before, mustDigest(t, recreated, baseStdioIdentity())); !slices.Equal(got, []string{"key"}) {
		t.Fatalf("labels after key loss = %q, want [key]", got)
	}
	if err := os.WriteFile(keyPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newPinStore(ws, base); err == nil {
		t.Fatal("corrupt key accepted")
	}
	if raw, _ := os.ReadFile(keyPath); string(raw) != "corrupt" {
		t.Fatal("corrupt key was regenerated")
	}
}

func TestConnectionFieldDigestsAreDomainSeparated(t *testing.T) {
	c := baseStdioIdentity()
	c.launcher, c.target, c.dir = "/same", "/same", "/same"
	p := mustDigest(t, testPins(t), c)
	if p.fields["launcher"] == p.fields["target"] || p.fields["target"] == p.fields["dir"] {
		t.Fatal("equal values in different fields produced equal tags; field domains are not separated")
	}
}

const fingerprintMarker = "mcpclient-fingerprint"

// TestMCPClientFingerprintHelper is not a test. Re-executed with
// fingerprintMarker followed by a workspace and a base directory, the test
// binary prints baseStdioIdentity's fingerprint under that store's key.
func TestMCPClientFingerprintHelper(t *testing.T) {
	i := slices.Index(os.Args, fingerprintMarker)
	if i < 0 || i+2 >= len(os.Args) {
		return
	}
	s, err := newPinStore(os.Args[i+1], os.Args[i+2])
	if err != nil {
		os.Exit(2)
	}
	p, err := s.digestConnection(context.Background(), baseStdioIdentity())
	if err != nil {
		os.Exit(3)
	}
	fmt.Print(p.fingerprint)
	os.Exit(0)
}

func TestConnectionFingerprintStableAcrossProcesses(t *testing.T) {
	ws, base := t.TempDir(), t.TempDir()
	s, err := newPinStore(ws, base)
	if err != nil {
		t.Fatal(err)
	}
	want := mustDigest(t, s, baseStdioIdentity()).fingerprint
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(exe, "-test.run=^TestMCPClientFingerprintHelper$", "--", fingerprintMarker, ws, base).Output()
	if err != nil {
		t.Fatalf("helper process: %v", err)
	}
	if got := string(out); got != want {
		t.Fatalf("fingerprint in another process = %q, want %q", got, want)
	}
}

func TestConnectionKeyUnreadableFailsWithoutRegenerating(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not block reads for this user/platform")
	}
	ws, base := t.TempDir(), t.TempDir()
	if _, err := newPinStore(ws, base); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(base, "golem", "mcp-pins", connectionKeyFile)
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(keyPath, 0o600) })
	if _, err := newPinStore(ws, base); err == nil {
		t.Fatal("unreadable key accepted")
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(keyPath); !bytes.Equal(before, after) {
		t.Fatal("unreadable key was regenerated")
	}
}

func TestConnectionPinCarriesNoValues(t *testing.T) {
	c := baseStdioIdentity()
	c.argv, c.dir, c.launcher = []string{"l", "--token=canary-argv"}, "/canary-dir", "/canary-launcher"
	p := mustDigest(t, testPins(t), c)
	if got := fmt.Sprintf("%+v", *p); strings.Contains(got, "canary") {
		t.Fatalf("pin carries identity values: %s", got)
	}
}
