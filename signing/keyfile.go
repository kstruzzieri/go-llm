package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyDirMode      = 0o700
	keyFileMode     = 0o600
	maxKeyFileBytes = 64 * 1024
	hmacFileKeySize = 32
	pemPrivateKey   = "PRIVATE KEY" // PKCS#8
	pemPublicKey    = "PUBLIC KEY"  // PKIX/RFC 8410
	pemHMACKey      = "HMAC-SHA256 KEY"
)

// syncKeyDirectory and syncKeyFile are injectable only for durability
// regression tests. Production always points at the platform implementation.
var (
	syncKeyDirectory = syncDirectory
	syncKeyFile      = func(f *os.File) error { return f.Sync() }
)

// LoadOrCreateEd25519 loads the PKCS#8 PEM Ed25519 private key at path,
// generating and atomically publishing one when the file does not exist.
// created reports identity creation so callers cannot mistake key loss for
// an ordinary load.
func LoadOrCreateEd25519(path string) (*Ed25519Signer, bool, error) {
	state, err := openKeyRootState(path, true)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = state.root.Close() }()
	raw, created, err := loadOrCreateKeyFile(state.root, state.name, state.syncParent, func() ([]byte, error) {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: pemPrivateKey, Bytes: der}), nil
	})
	if err != nil {
		return nil, created, err
	}
	signer, err := parseEd25519Signer(raw, path)
	return signer, created, err
}

// LoadOrCreateHMAC loads one HMAC-SHA256 KEY PEM block, generating a
// 32-byte random key when the file does not exist. created reports identity
// creation so callers cannot mistake key loss for an ordinary load.
func LoadOrCreateHMAC(path string) (*HMACSigner, bool, error) {
	state, err := openKeyRootState(path, true)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = state.root.Close() }()
	raw, created, err := loadOrCreateKeyFile(state.root, state.name, state.syncParent, func() ([]byte, error) {
		key := make([]byte, hmacFileKeySize)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: pemHMACKey, Bytes: key}), nil
	})
	if err != nil {
		return nil, created, err
	}
	signer, err := parseHMACSigner(raw, path)
	return signer, created, err
}

// LoadEd25519 loads an existing PKCS#8 PEM Ed25519 private key without
// creating anything or syncing its directory.
func LoadEd25519(path string) (*Ed25519Signer, error) {
	raw, err := loadKeyFile(path)
	if err != nil {
		return nil, err
	}
	return parseEd25519Signer(raw, path)
}

// LoadHMAC loads an existing HMAC-SHA256 KEY PEM block without creating
// anything or syncing its directory.
func LoadHMAC(path string) (*HMACSigner, error) {
	raw, err := loadKeyFile(path)
	if err != nil {
		return nil, err
	}
	return parseHMACSigner(raw, path)
}

// LoadEd25519Verifier loads an existing PKIX/RFC 8410 Ed25519 public key PEM
// without creating anything or syncing its directory.
func LoadEd25519Verifier(path string) (*Ed25519Verifier, error) {
	raw, err := loadKeyFile(path)
	if err != nil {
		return nil, err
	}
	return parseEd25519Verifier(raw, path)
}

func parseEd25519Signer(raw []byte, path string) (*Ed25519Signer, error) {
	der, err := decodeKeyPEM(raw, path, pemPrivateKey)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("signing: %s: parse PKCS#8: %w", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing: %s: key is %T, want ed25519.PrivateKey", path, key)
	}
	return NewEd25519Signer(priv)
}

func parseHMACSigner(raw []byte, path string) (*HMACSigner, error) {
	key, err := decodeKeyPEM(raw, path, pemHMACKey)
	if err != nil {
		return nil, err
	}
	if len(key) != hmacFileKeySize {
		return nil, fmt.Errorf("signing: %s: HMAC key is %d bytes, want %d", path, len(key), hmacFileKeySize)
	}
	return NewHMAC(key)
}

func parseEd25519Verifier(raw []byte, path string) (*Ed25519Verifier, error) {
	der, err := decodeKeyPEM(raw, path, pemPublicKey)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("signing: %s: parse PKIX public key: %w", path, err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("signing: %s: key is %T, want ed25519.PublicKey", path, key)
	}
	return NewEd25519Verifier(pub)
}

func loadKeyFile(path string) ([]byte, error) {
	state, err := openKeyRootState(path, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = state.root.Close() }()
	raw, exists, err := readKeyFile(state.root, state.name, nil)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("signing: key file %s: %w", path, fs.ErrNotExist)
	}
	return raw, nil
}

// openKeyRoot creates at most the dedicated owner-only leaf directory (its
// parent must exist), anchors it with os.Root, and proves the opened root is
// the directory Lstat saw.
func openKeyRoot(path string) (*os.Root, string, error) {
	state, err := openKeyRootState(path, true)
	if err != nil {
		return nil, "", err
	}
	return state.root, state.name, nil
}

type keyRootState struct {
	root       *os.Root
	name       string
	syncParent bool
}

func openKeyRootState(path string, create bool) (keyRootState, error) {
	if strings.TrimSpace(path) == "" {
		return keyRootState{}, errors.New("signing: key path is empty")
	}
	clean := filepath.Clean(path)
	dir, name := filepath.Dir(clean), filepath.Base(clean)
	if filepath.Base(path) == ".." || name == ".." {
		return keyRootState{}, fmt.Errorf("signing: key path %q must not name parent directory", path)
	}
	if name == "." || name == string(filepath.Separator) {
		return keyRootState{}, fmt.Errorf("signing: key path %q does not name a file", path)
	}
	dirInfo, err := os.Lstat(dir)
	syncParent := false
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return keyRootState{}, fmt.Errorf("signing: inspect key directory %s: %w", dir, err)
		}
		syncParent = true
		// No os.Chmod(dir) afterwards: a path-based chmod follows a symlink an
		// ancestor-writer can swap in between Mkdir and Chmod, which would be an
		// arbitrary-path chmod as this UID. Mkdir's mode already yields an
		// owner-only directory; a umask that strips owner bits leaves it unusable
		// and creation fails closed at the temp write instead.
		switch mkdirErr := os.Mkdir(dir, keyDirMode); {
		case mkdirErr == nil, errors.Is(mkdirErr, fs.ErrExist): // created, or a concurrent creator won
		default:
			return keyRootState{}, fmt.Errorf("signing: create key directory %s: %w", dir, mkdirErr)
		}
		dirInfo, err = os.Lstat(dir)
	}
	if err != nil {
		return keyRootState{}, fmt.Errorf("signing: inspect key directory %s: %w", dir, err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() || !ownerAndModeOK(dirInfo) {
		return keyRootState{}, fmt.Errorf("%w: %s (mode %v)", ErrInsecureKeyDirectory, dir, dirInfo.Mode())
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return keyRootState{}, fmt.Errorf("signing: open key directory %s: %w", dir, err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(dirInfo, opened) {
		_ = root.Close()
		return keyRootState{}, fmt.Errorf("%w: %s changed while opening", ErrInsecureKeyDirectory, dir)
	}
	return keyRootState{root: root, name: name, syncParent: syncParent}, nil
}

// loadOrCreateKeyFile publishes only complete synced bytes. A hard link is
// the atomic no-replace commit: a racing loser can read only the winner's
// complete temp, never a partially written final file.
func loadOrCreateKeyFile(root *os.Root, name string, syncParent bool, generate func() ([]byte, error)) ([]byte, bool, error) {
	if syncParent {
		if err := syncKeyDirectoryParent(root); err != nil {
			return nil, false, err
		}
	}
	raw, exists, err := loadExistingKeyFile(root, name)
	if err != nil {
		return nil, false, err
	}
	if exists {
		return raw, false, nil
	}
	if !syncParent {
		if err := syncKeyDirectoryParent(root); err != nil {
			return nil, false, err
		}
	}
	material, err := generate()
	if err != nil {
		return nil, false, fmt.Errorf("signing: generate key: %w", err)
	}
	temp, err := writeKeyTemp(root, name, material)
	if err != nil {
		return nil, false, err
	}
	linkErr := root.Link(temp, name)
	removeErr := root.Remove(temp)
	if linkErr != nil {
		wrapped := fmt.Errorf("signing: publish key file %s: %w", filepath.Join(root.Name(), name), linkErr)
		if removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			wrapped = errors.Join(wrapped, fmt.Errorf("signing: remove key temp %s: %w", temp, removeErr))
		}
		if !errors.Is(linkErr, fs.ErrExist) || removeErr != nil {
			return nil, false, wrapped
		}
		raw, exists, err := loadExistingKeyFile(root, name)
		if err != nil {
			return nil, false, err
		}
		if !exists {
			return nil, false, errors.New("signing: concurrently published key disappeared")
		}
		return raw, false, nil
	}
	var postPublish []error
	if removeErr != nil {
		postPublish = append(postPublish, fmt.Errorf("signing: remove published key temp %s: %w", temp, removeErr))
	}
	if err := syncKeyDirectory(root.Name()); err != nil {
		postPublish = append(postPublish,
			fmt.Errorf("%w: %s: %w", ErrKeyFileDurability, filepath.Join(root.Name(), name), err))
	}
	if len(postPublish) > 0 {
		return material, true, errors.Join(postPublish...)
	}
	return material, true, nil
}

func syncKeyDirectoryParent(root *os.Root) error {
	parent := filepath.Dir(root.Name())
	if err := syncKeyDirectory(parent); err != nil {
		return fmt.Errorf("%w: sync key directory parent %s: %w", ErrKeyFileDurability, parent, err)
	}
	return nil
}

// loadExistingKeyFile reads a published key and syncs the key directory
// before trusting it. The creator that published the file may have failed
// its own directory sync (ErrKeyFileDurability), in which case the entry is
// not yet known to be durable; without this sync a plain retry would report
// success while a crash could still drop the file and rotate the identity.
func loadExistingKeyFile(root *os.Root, name string) ([]byte, bool, error) {
	raw, exists, err := readKeyFile(root, name, nil)
	if err != nil || !exists {
		return nil, exists, err
	}
	if err := syncKeyDirectory(root.Name()); err != nil {
		return nil, true, fmt.Errorf("%w: %s: %w", ErrKeyFileDurability, filepath.Join(root.Name(), name), err)
	}
	return raw, true, nil
}

func writeKeyTemp(root *os.Root, name string, material []byte) (temp string, err error) {
	temp = "." + name + ".tmp-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if err != nil {
		return "", fmt.Errorf("signing: create key temp: %w", err)
	}
	closed := false
	defer func() {
		if err == nil {
			return
		}
		var cleanup []error
		if !closed {
			if closeErr := f.Close(); closeErr != nil {
				cleanup = append(cleanup, fmt.Errorf("close key temp: %w", closeErr))
			}
		}
		if removeErr := root.Remove(temp); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			cleanup = append(cleanup, fmt.Errorf("remove key temp: %w", removeErr))
		}
		if len(cleanup) > 0 {
			err = errors.Join(append([]error{err}, cleanup...)...)
		}
	}()
	if err = f.Chmod(keyFileMode); err != nil {
		return temp, fmt.Errorf("signing: chmod key temp: %w", err)
	}
	if _, err = f.Write(material); err != nil {
		return temp, fmt.Errorf("signing: write key temp: %w", err)
	}
	if err = syncKeyFile(f); err != nil {
		return temp, fmt.Errorf("signing: sync key temp: %w", err)
	}
	err = f.Close()
	closed = true
	if err != nil {
		return temp, fmt.Errorf("signing: close key temp: %w", err)
	}
	return temp, nil
}

// readKeyFile distinguishes true first-use absence from disappearance after
// Lstat. afterLstat is nil in production and deterministic in the swap tests.
func readKeyFile(root *os.Root, name string, afterLstat func()) ([]byte, bool, error) {
	path := filepath.Join(root.Name(), name)
	before, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("signing: lstat key file %s: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, true, fmt.Errorf("%w: %s (mode %v)", ErrKeyFileNotRegular, path, before.Mode().Type())
	}
	if afterLstat != nil {
		afterLstat()
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, true, fmt.Errorf("signing: open existing key file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	after, err := f.Stat()
	if err != nil {
		return nil, true, fmt.Errorf("signing: stat key file %s: %w", path, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, true, fmt.Errorf("%w: %s changed between lstat and open", ErrKeyFileNotRegular, path)
	}
	if !ownerAndModeOK(after) {
		return nil, true, fmt.Errorf("%w: %s (mode %04o)", ErrInsecureKeyFile, path, after.Mode().Perm())
	}
	if after.Size() > maxKeyFileBytes {
		return nil, true, fmt.Errorf("signing: key file %s is %d bytes, cap %d", path, after.Size(), maxKeyFileBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, true, fmt.Errorf("signing: read key file %s: %w", path, err)
	}
	if len(raw) > maxKeyFileBytes {
		return nil, true, fmt.Errorf("signing: key file %s exceeds %d bytes", path, maxKeyFileBytes)
	}
	return raw, true, nil
}

// decodeKeyPEM accepts exactly one canonical PEM block of wantType and nothing
// else. pem.Decode skips a BEGIN line it cannot parse and returns the next
// block of any type, so the first line, the decoded block's type, and the
// count of BEGIN markers are all checked; otherwise a file starting with the
// right BEGIN line could carry a foreign block or unvalidated text.
// The block-type comparison is implied by the first two conditions and is
// kept as the direct statement of intent (a mutation deleting it is equivalent).
func decodeKeyPEM(raw []byte, path string, wantType string) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	block, rest := pem.Decode(trimmed)
	if bytes.Count(trimmed, []byte("-----BEGIN ")) != 1 ||
		!bytes.HasPrefix(trimmed, []byte("-----BEGIN "+wantType+"-----")) ||
		block == nil || block.Type != wantType ||
		len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("signing: %s: want exactly one %q PEM block without headers or surrounding data", path, wantType)
	}
	return block.Bytes, nil
}
