package signing

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func secureKeyDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadOrCreateEd25519RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "agent.pem")
	s1, created, err := LoadOrCreateEd25519(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first call did not report key creation")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		t.Fatalf("file is not a single PRIVATE KEY PEM block: %q", raw)
	}
	s2, created, err := LoadOrCreateEd25519(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second call reported a new identity")
	}
	if s1.KeyID() != s2.KeyID() {
		t.Fatalf("second load produced a different key: %s vs %s", s1.KeyID(), s2.KeyID())
	}
}

func TestLoadOrCreateHMACRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "hmac.pem")
	m1, created, err := LoadOrCreateHMAC(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first call did not report key creation")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "HMAC-SHA256 KEY" || len(rest) != 0 || len(block.Bytes) != 32 {
		t.Fatalf("hmac file is not one 32-byte HMAC-SHA256 KEY block: %q", raw)
	}
	m2, created, err := LoadOrCreateHMAC(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second call reported a new identity")
	}
	if m1.KeyID() != m2.KeyID() {
		t.Fatal("second load produced a different key")
	}
}

func TestLoadOrCreateHMACConcurrentCreatorsConverge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "hmac.pem")
	const workers = 16
	type result struct {
		kid     string
		created bool
		err     error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			signer, created, err := LoadOrCreateHMAC(path)
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{kid: signer.KeyID(), created: created}
		}()
	}
	wg.Wait()
	close(results)
	created := 0
	wantKID := ""
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.created {
			created++
		}
		if wantKID == "" {
			wantKID = result.kid
		} else if result.kid != wantKID {
			t.Fatalf("concurrent creators returned different keys: %s vs %s", result.kid, wantKID)
		}
	}
	if created != 1 {
		t.Fatalf("created count = %d, want 1", created)
	}
}

func TestLoadOrCreateReportsPostPublishDurabilityFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "hmac.pem")
	keyDir := filepath.Dir(path)
	original := syncKeyDirectory
	syncKeyDirectory = func(dir string) error {
		if dir == keyDir {
			return errors.New("injected sync failure")
		}
		return original(dir)
	}
	t.Cleanup(func() { syncKeyDirectory = original })

	if _, created, err := LoadOrCreateHMAC(path); !created || !errors.Is(err, ErrKeyFileDurability) {
		t.Fatalf("first call = created %v, err %v; want created + ErrKeyFileDurability", created, err)
	}
	syncKeyDirectory = original
	if _, created, err := LoadOrCreateHMAC(path); err != nil || created {
		t.Fatalf("retry = created %v, err %v; want existing complete key", created, err)
	}
}

func TestLoadOrCreateWritesNothingBeforeParentSync(t *testing.T) {
	// Generation itself happens in memory and is unobservable; the durability
	// property that matters is that no temp or final file exists in the key
	// directory until its parent entry has been synced.
	base := t.TempDir()
	path := filepath.Join(base, "keys", "hmac.pem")
	original := syncKeyDirectory
	failed := false
	syncKeyDirectory = func(dir string) error {
		if dir == base && !failed {
			failed = true
			return errors.New("injected parent sync failure")
		}
		return original(dir)
	}
	t.Cleanup(func() { syncKeyDirectory = original })

	if _, created, err := LoadOrCreateHMAC(path); created || !errors.Is(err, ErrKeyFileDurability) {
		t.Fatalf("first call = created %v, err %v; want not-created + ErrKeyFileDurability", created, err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 0 {
		t.Fatalf("key directory written before parent sync: entries %v, err %v", entries, err)
	}
	syncKeyDirectory = original
	if _, created, err := LoadOrCreateHMAC(path); err != nil || !created {
		t.Fatalf("retry = created %v, err %v; want newly created key", created, err)
	}
}

func TestKeyDirectoryRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, _, err := LoadOrCreateHMAC(filepath.Join(link, "hmac.pem")); !errors.Is(err, ErrInsecureKeyDirectory) {
		t.Fatalf("symlinked key directory: err = %v, want ErrInsecureKeyDirectory", err)
	}
}

func TestKeyFileRejectsSymlink(t *testing.T) {
	dir := secureKeyDir(t)
	real := filepath.Join(dir, "real.pem")
	if _, _, err := LoadOrCreateEd25519(real); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(real)
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, _, err := LoadOrCreateEd25519(link); !errors.Is(err, ErrKeyFileNotRegular) {
		t.Fatalf("symlink: err = %v, want ErrKeyFileNotRegular", err)
	}
	after, _ := os.ReadFile(real)
	if string(before) != string(after) {
		t.Fatal("symlink target was rewritten")
	}
}

func TestReadKeyFileRejectsSwapAfterLstat(t *testing.T) {
	dir := secureKeyDir(t)
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "replacement.pem"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, name, err := openKeyRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	_, existed, err := readKeyFile(root, name, func() {
		if err := root.Remove(name); err != nil {
			t.Fatal(err)
		}
		if err := root.Rename("replacement.pem", name); err != nil {
			t.Fatal(err)
		}
	})
	if !existed || !errors.Is(err, ErrKeyFileNotRegular) {
		t.Fatalf("swapped key = existed %v, err %v; want ErrKeyFileNotRegular", existed, err)
	}
}

func TestReadKeyFileDoesNotTurnPostLstatDisappearanceIntoFirstUse(t *testing.T) {
	dir := secureKeyDir(t)
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, name, err := openKeyRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	_, existed, err := readKeyFile(root, name, func() {
		if err := root.Remove(name); err != nil {
			t.Fatal(err)
		}
	})
	if !existed || err == nil {
		t.Fatalf("disappeared key = existed %v, err %v; want existing-path failure", existed, err)
	}
}

func TestKeyFileRejectsDirectory(t *testing.T) {
	dir := filepath.Join(secureKeyDir(t), "directory-not-key")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateEd25519(dir); !errors.Is(err, ErrKeyFileNotRegular) {
		t.Fatalf("directory: err = %v, want ErrKeyFileNotRegular", err)
	}
}

func TestKeyFileRejectsWrongContent(t *testing.T) {
	dir := secureKeyDir(t)
	write := func(name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, _, err := LoadOrCreateEd25519(write("garbage.pem", []byte("not pem"))); err == nil {
		t.Error("garbage accepted as ed25519")
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, _, err := LoadOrCreateEd25519(write("ec.pem", ecPEM)); err == nil || !strings.Contains(err.Error(), "want ed25519.PrivateKey") {
		t.Errorf("ecdsa key: err = %v, want 'want ed25519.PrivateKey'", err)
	}
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edDER, err := x509.MarshalPKCS8PrivateKey(edPriv)
	if err != nil {
		t.Fatal(err)
	}
	validEdPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: edDER})
	two := append(append([]byte{}, validEdPEM...), validEdPEM...)
	if _, _, err := LoadOrCreateEd25519(write("two.pem", two)); err == nil {
		t.Error("two PEM blocks accepted")
	}
	validHMAC := pem.EncodeToMemory(&pem.Block{Type: "HMAC-SHA256 KEY", Bytes: make([]byte, 32)})
	withJunk := append([]byte("not part of the PEM\n"), validHMAC...)
	if _, _, err := LoadOrCreateHMAC(write("leading-junk.pem", withJunk)); err == nil {
		t.Error("leading junk before PEM accepted")
	}
	shortHMAC := pem.EncodeToMemory(&pem.Block{Type: "HMAC-SHA256 KEY", Bytes: make([]byte, 16)})
	if _, _, err := LoadOrCreateHMAC(write("short.pem", shortHMAC)); err == nil {
		t.Error("16-byte hmac key PEM accepted")
	}
	wrongTypeHMAC := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: make([]byte, 32)})
	if _, _, err := LoadOrCreateHMAC(write("wrong-type.pem", wrongTypeHMAC)); err == nil {
		t.Error("PRIVATE KEY PEM accepted as HMAC")
	}
	withHeaders := pem.EncodeToMemory(&pem.Block{
		Type:    "HMAC-SHA256 KEY",
		Headers: map[string]string{"Comment": "not canonical"},
		Bytes:   make([]byte, 32),
	})
	if _, _, err := LoadOrCreateHMAC(write("headers.pem", withHeaders)); err == nil {
		t.Error("PEM headers accepted")
	}
	if _, _, err := LoadOrCreateHMAC(write("raw.key", make([]byte, 32))); err == nil {
		t.Error("untyped raw HMAC key accepted")
	}
	// Literal on purpose: the boundary must not move with the constant under
	// test. Three guards overlap (stat size, LimitReader, length check), so only
	// the constant itself is individually killable; that redundancy is intended.
	oversized := append(bytes.Clone(validHMAC), bytes.Repeat([]byte(" "), 64*1024+1-len(validHMAC))...)
	if _, _, err := LoadOrCreateHMAC(write("big.key", oversized)); err == nil {
		t.Error("oversized key file accepted")
	}
}

// A publish whose directory sync failed leaves the final key file in place.
// The next load must not accept that file as durable until a directory sync
// succeeds; otherwise a plain retry clears ErrKeyFileDurability while a crash
// could still drop the entry and silently rotate the identity.
func TestLoadOrCreateExistingKeyRetriesDirectorySync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "hmac.pem")
	keyDir := filepath.Dir(path)
	original := syncKeyDirectory
	failing := true
	syncKeyDirectory = func(dir string) error {
		if dir == keyDir && failing {
			return errors.New("injected sync failure")
		}
		return original(dir)
	}
	t.Cleanup(func() { syncKeyDirectory = original })

	if _, created, err := LoadOrCreateHMAC(path); !created || !errors.Is(err, ErrKeyFileDurability) {
		t.Fatalf("first call = created %v, err %v; want created + ErrKeyFileDurability", created, err)
	}
	published, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := LoadOrCreateHMAC(path); created || !errors.Is(err, ErrKeyFileDurability) {
		t.Fatalf("retry while sync still fails = created %v, err %v; want not-created + ErrKeyFileDurability", created, err)
	}
	failing = false
	signer, created, err := LoadOrCreateHMAC(path)
	if err != nil || created {
		t.Fatalf("retry after sync recovers = created %v, err %v; want existing key", created, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, published) {
		t.Fatalf("key file changed across retries: err %v", err)
	}
	if signer == nil {
		t.Fatal("recovered load returned nil signer")
	}
}

// The loser of a publish race reads the winner's complete file, but the
// winner's own directory sync may have failed. The loser must sync the key
// directory before reporting the key as durably stored.
func TestLoadOrCreateConcurrentLoserSyncsKeyDirectory(t *testing.T) {
	keyDir := secureKeyDir(t)
	const name = "hmac.pem"
	winner := []byte("-----BEGIN HMAC-SHA256 KEY-----\nAAAA\n-----END HMAC-SHA256 KEY-----\n")
	original := syncKeyDirectory
	syncKeyDirectory = func(dir string) error {
		if dir == keyDir {
			return errors.New("injected sync failure")
		}
		return original(dir)
	}
	t.Cleanup(func() { syncKeyDirectory = original })
	root, err := os.OpenRoot(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	raw, created, err := loadOrCreateKeyFile(root, name, func() ([]byte, error) {
		// The winner publishes between this creator's absence check and its link.
		if err := os.WriteFile(filepath.Join(keyDir, name), winner, 0o600); err != nil {
			return nil, err
		}
		return []byte("loser material"), nil
	})
	if created || !errors.Is(err, ErrKeyFileDurability) {
		t.Fatalf("loser = created %v, raw %q, err %v; want not-created + ErrKeyFileDurability", created, raw, err)
	}
	if got, err := os.ReadFile(filepath.Join(keyDir, name)); err != nil || !bytes.Equal(got, winner) {
		t.Fatalf("winner's key file disturbed: %q, err %v", got, err)
	}
	entries, err := os.ReadDir(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("key directory has %d entries after loser cleanup, want 1", len(entries))
	}
}

func TestKeyFileRejectsForeignBlockBehindMatchingBeginLine(t *testing.T) {
	dir := secureKeyDir(t)
	write := func(name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edDER, err := x509.MarshalPKCS8PrivateKey(edPriv)
	if err != nil {
		t.Fatal(err)
	}
	edBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: edDER})
	hmacBlock := pem.EncodeToMemory(&pem.Block{Type: "HMAC-SHA256 KEY", Bytes: make([]byte, 32)})

	// pem.Decode skips a BEGIN line it cannot parse and returns the next block
	// of any type; the file's first line alone must not decide the type.
	typeSwapHMAC := append([]byte("-----BEGIN HMAC-SHA256 KEY-----\n"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: make([]byte, 32)})...)
	if _, _, err := LoadOrCreateHMAC(write("type-swap.pem", typeSwapHMAC)); err == nil {
		t.Error("PRIVATE KEY block accepted behind an HMAC-SHA256 KEY BEGIN line")
	}
	typeSwapEd := append([]byte("-----BEGIN PRIVATE KEY-----\n"), pem.EncodeToMemory(&pem.Block{Type: "HMAC-SHA256 KEY", Bytes: edDER})...)
	if _, _, err := LoadOrCreateEd25519(write("type-swap-ed.pem", typeSwapEd)); err == nil {
		t.Error("HMAC-SHA256 KEY block accepted behind a PRIVATE KEY BEGIN line")
	}
	junkThenReal := append([]byte("-----BEGIN PRIVATE KEY-----\nATTACKER NOTE: never validated\n"), edBlock...)
	if _, _, err := LoadOrCreateEd25519(write("junk-then-real.pem", junkThenReal)); err == nil {
		t.Error("unvalidated text between a BEGIN line and the real block accepted")
	}
	if _, _, err := LoadOrCreateHMAC(write("real-hmac.pem", hmacBlock)); err != nil {
		t.Fatalf("genuine HMAC PEM rejected: %v", err)
	}
	if _, _, err := LoadOrCreateEd25519(write("real-ed.pem", edBlock)); err != nil {
		t.Fatalf("genuine Ed25519 PEM rejected: %v", err)
	}
}

func TestKeyFileRejectsOverlongHMACKey(t *testing.T) {
	dir := secureKeyDir(t)
	p := filepath.Join(dir, "long.pem")
	long := pem.EncodeToMemory(&pem.Block{Type: "HMAC-SHA256 KEY", Bytes: make([]byte, 64)})
	if err := os.WriteFile(p, long, 0o600); err != nil {
		t.Fatal(err)
	}
	// NewHMAC would accept 64 bytes; only the file format's exact-length rule
	// (D9) can reject this.
	_, _, err := LoadOrCreateHMAC(p)
	if err == nil || !strings.Contains(err.Error(), "HMAC key is 64 bytes, want 32") {
		t.Fatalf("64-byte HMAC PEM: err = %v, want the exact-length rejection", err)
	}
}

func TestKeyDirectoryRejectsRegularFile(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateHMAC(filepath.Join(notDir, "hmac.pem")); !errors.Is(err, ErrInsecureKeyDirectory) {
		t.Fatalf("regular file at the key directory position: err = %v, want ErrInsecureKeyDirectory", err)
	}
}

func TestLoadOrCreateFailedTempSyncPublishesNothingAndIsRetryable(t *testing.T) {
	dir := secureKeyDir(t)
	path := filepath.Join(dir, "hmac.pem")
	original := syncKeyFile
	syncKeyFile = func(*os.File) error { return errors.New("injected temp sync failure") }
	t.Cleanup(func() { syncKeyFile = original })

	_, created, err := LoadOrCreateHMAC(path)
	if err == nil || created || !strings.Contains(err.Error(), "sync key temp") {
		t.Fatalf("failed temp sync = created %v, err %v; want not-created + temp sync error", created, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("failed publication left %v behind; want an empty key directory", names)
	}
	syncKeyFile = original
	if _, created, err := LoadOrCreateHMAC(path); err != nil || !created {
		t.Fatalf("retry = created %v, err %v; want a newly created key", created, err)
	}
}
