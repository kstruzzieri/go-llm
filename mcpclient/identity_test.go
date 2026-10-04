package mcpclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/signing"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
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
		{func(c *connectionIdentity) { c.workspace = "/other" }, []string{"identity"}},
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
	httpID := connectionIdentity{workspace: "/ws", alias: "fs", kind: "http", origin: "https://h", endpoint: "/"}
	if got := compareConnectionPins(before, mustDigest(t, recreated, httpID)); !slices.Equal(got, []string{"kind", "key"}) {
		t.Fatalf("labels after key loss and kind change = %q, want [kind key]", got)
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

// TestConnectionFingerprintKnownAnswer pins the persisted encoding: payloads
// are literal canonical JSON and the signing frame is rebuilt by hand.
func TestConnectionFingerprintKnownAnswer(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	k, err := signing.NewHMAC(key)
	if err != nil {
		t.Fatal(err)
	}
	store := &PinStore{connKey: k}
	// "go-llm-signing-v1\x00" || uint64 big-endian len(domain) || domain || payload
	tag := func(domain, payload string) string {
		frame := binary.BigEndian.AppendUint64([]byte("go-llm-signing-v1\x00"), uint64(len(domain)))
		frame = append(frame, domain+payload...)
		m := hmac.New(sha256.New, key)
		_, _ = m.Write(frame)
		return hex.EncodeToString(m.Sum(nil))
	}
	stdio := mustDigest(t, store, baseStdioIdentity())
	if want := "hmac-sha256:" + tag("go-llm/mcp-connection/v1", `{"alias":"fs","argv":["l","--a"],"dir":"/w","env":["inherit:TOKEN"],"env_baseline":"unix-v1","format":1,"kind":"stdio","launcher":"/bin/l","target":"/bin/t","workspace":"/ws"}`); stdio.fingerprint != want {
		t.Errorf("stdio fingerprint = %s, want %s", stdio.fingerprint, want)
	}
	if want := tag("go-llm/mcp-connection-field/v1/env_baseline", `"unix-v1"`); stdio.fields["env_baseline"] != want {
		t.Errorf("env_baseline tag = %s, want %s", stdio.fields["env_baseline"], want)
	}
	http := mustDigest(t, store, connectionIdentity{workspace: "/ws", alias: "api", kind: "http", origin: "https://h", endpoint: "/mcp?q=1"})
	if want := "hmac-sha256:" + tag("go-llm/mcp-connection/v1", `{"alias":"api","endpoint":"/mcp?q=1","format":1,"kind":"http","origin":"https://h","workspace":"/ws"}`); http.fingerprint != want {
		t.Errorf("http fingerprint = %s, want %s", http.fingerprint, want)
	}
}

// TestConnectionInvalidUTF8IsLaunchInvalid: identity values that cannot be
// fingerprinted block the alias as an unusable launch, before any dial, and
// the refusal never echoes the value.
func TestConnectionInvalidUTF8IsLaunchInvalid(t *testing.T) {
	pins := testPins(t)
	s, _, _ := staticCatalogServer(t, "fs", &gomcp.Tool{Name: "read", Description: "A"})
	s.command = []string{"l", "--tok=\xffcanary"}
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{s}, ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if a := admission(t, w); a.Reason != "launch_invalid" || len(m.Tools()) != 0 || strings.Contains(fmt.Sprint(w), "canary") {
		t.Fatalf("connect: reason %q, %d tools, warnings %v", a.Reason, len(m.Tools()), w)
	}
	_, err = Inspect(context.Background(), Implementation{Name: "test"}, s, pins)
	var a *AdmissionError
	if !errors.As(err, &a) || a.Reason != "launch_invalid" || strings.Contains(err.Error(), "canary") {
		t.Fatalf("inspect: %v", err)
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
