package memory

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/kstruzzieri/go-llm/signing"
)

// RecordWriter identifies the host entry point accepting ordinary records.
type RecordWriter uint8

const (
	// WriterDirect is the ordinary library surface.
	WriterDirect RecordWriter = iota
	// WriterGolem is Golem's agent-memory surface.
	WriterGolem
	// WriterMCP is MCP's agent-memory surface.
	WriterMCP
)

// RecordStoreConfig supplies either an injected signer/ring pair or KeyDir.
// Injected hosts must finish populating Verifiers before construction and never
// mutate it while the store is in use. Initialize authorizes the initial import
// only with injected credentials; omit it on subsequent opens.
type RecordStoreConfig struct {
	Signer     signing.Signer
	Verifiers  *signing.Keyring
	KeyDir     string
	Writer     RecordWriter
	Initialize bool
}

// ErrIncompleteInitialization requires operator recovery: the filesystem
// identity exists but the database has no committed signing initialization.
var ErrIncompleteInitialization = errors.New("memory: signing initialization incomplete; operator recovery required")

func (c RecordStoreConfig) origin() (string, error) {
	switch c.Writer {
	case WriterDirect:
		return "memory.create", nil
	case WriterGolem:
		return "golem.agent_memory_create", nil
	case WriterMCP:
		return "mcp.agent_memory_create", nil
	default:
		return "", errors.New("memory: unknown record writer")
	}
}

func (c RecordStoreConfig) validate() error {
	if _, err := c.origin(); err != nil {
		return err
	}
	if c.KeyDir != "" {
		if c.Signer != nil || c.Verifiers != nil || c.Initialize {
			return errors.New("memory: conflicting record signing configuration")
		}
		return nil
	}
	if c.Signer == nil || c.Verifiers == nil {
		return errors.New("memory: record signer and verifiers are required")
	}
	v := reflect.ValueOf(c.Signer)
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return errors.New("memory: nil record signer")
	}
	return nil
}

const recordInitializedQuery = `SELECT EXISTS(SELECT 1 FROM memory_record_signing WHERE id = 1)`

func (s *MemoryRecordStore) initializeSigning(ctx context.Context, config RecordStoreConfig) error {
	var initialized bool
	if err := s.db.QueryRowContext(ctx, recordInitializedQuery).Scan(&initialized); err != nil {
		return fmt.Errorf("memory: inspect signing initialization: %w", err)
	}
	if initialized {
		_, err := s.configureSigning(ctx, config, true)
		return err
	}
	// The marker row is the claim (#631), and inserting it must be the
	// transaction's first statement: a first write waits for SQLite's write
	// lock under the busy timeout, while a write after a read fails at once
	// when another connection holds that lock. A concurrent opener therefore
	// waits here instead of finding a key with no marker, and an uncommitted
	// claim is invisible, so key-without-marker still means a crashed or
	// failed initialization. Until commit nothing may use s.db: the claim
	// holds the store's only connection on the OpenRecordStore path, and a
	// caller's handle would block on the lock the claim holds.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: initialize record signing: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	claim, err := tx.ExecContext(ctx, `INSERT INTO memory_record_signing (id, initialized_at) VALUES (1, ?) ON CONFLICT(id) DO NOTHING`, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("memory: initialize record signing: %w", err)
	}
	claimed, err := claim.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: initialize record signing: %w", err)
	}
	if claimed == 0 {
		// Another opener committed initialization while this one waited.
		if err := tx.Rollback(); err != nil {
			return fmt.Errorf("memory: initialize record signing: rollback: %w", err)
		}
		_, err := s.configureSigning(ctx, config, true)
		return err
	}
	created, err := s.configureSigning(ctx, config, false)
	if err != nil {
		return err
	}
	if err := s.importLegacyRecords(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit record import: %w", err)
	}
	if created {
		s.createdKeyID = s.signer.KeyID()
	}
	return nil
}

// configureSigning loads or creates the filesystem identity, or takes the
// injected pair, challenges it, and installs it on s. It reports whether it
// created the identity. It must not use s.db; see initializeSigning.
func (s *MemoryRecordStore) configureSigning(ctx context.Context, config RecordStoreConfig, initialized bool) (bool, error) {
	if initialized && config.Initialize {
		return false, errors.New("memory: record signing initialization already complete")
	}
	var created bool
	if config.KeyDir != "" {
		currentPath := filepath.Join(config.KeyDir, "current.pem")
		var current *signing.Ed25519Signer
		var err error
		if initialized {
			current, err = signing.LoadEd25519(currentPath)
		} else {
			current, created, err = signing.LoadOrCreateEd25519(currentPath)
		}
		if err != nil {
			return false, fmt.Errorf("memory: load signing identity: %w", err)
		}
		if !initialized && !created {
			return false, ErrIncompleteInitialization
		}
		config.Signer = current
		config.Verifiers, err = loadRecordVerifiers(config.KeyDir, current.Verifier())
		if err != nil {
			return false, err
		}
	}
	// Challenge the actual signer and ring, including the signature's binding to
	// the configured current identity, before importing or exposing any records.
	challenge := []byte(`{"purpose":"memory-record-configuration"}`)
	sig, err := config.Signer.Sign(ctx, MemoryRecordDomain, challenge)
	if err != nil {
		return false, fmt.Errorf("memory: signing configuration: %w", err)
	}
	if config.Signer.KeyID() == "" || config.Signer.Algorithm() == "" || sig.KeyID != config.Signer.KeyID() || sig.Alg != config.Signer.Algorithm() {
		return false, fmt.Errorf("memory: signing configuration: %w", signing.ErrKeyMismatch)
	}
	if err := config.Verifiers.Verify(ctx, MemoryRecordDomain, challenge, sig); err != nil {
		return false, fmt.Errorf("memory: signing configuration: %w", err)
	}
	s.signer, s.verifiers = config.Signer, config.Verifiers
	if !initialized && !created && !config.Initialize {
		return false, errors.New("memory: initial record import authorization required")
	}
	return created, nil
}

func loadRecordVerifiers(keyDir string, current *signing.Ed25519Verifier) (*signing.Keyring, error) {
	ring, err := signing.NewKeyring(current)
	if err != nil {
		return nil, err
	}
	trusted := filepath.Join(keyDir, "trusted")
	info, err := os.Lstat(trusted)
	if errors.Is(err, os.ErrNotExist) {
		return ring, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: inspect retained verifiers: %w", err)
	}
	if !info.IsDir() || !recordKeyDirectoryPrivate(info) {
		return nil, signing.ErrInsecureKeyDirectory
	}
	entries, err := os.ReadDir(trusted)
	if err != nil {
		return nil, fmt.Errorf("memory: list retained verifiers: %w", err)
	}
	for _, entry := range entries {
		v, err := signing.LoadEd25519Verifier(filepath.Join(trusted, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("memory: load retained verifier: %w", err)
		}
		if entry.Name() != v.KeyID()+".pem" {
			return nil, errors.New("memory: retained verifier filename does not match key identity")
		}
		if v.Algorithm() == current.Algorithm() && v.KeyID() == current.KeyID() && bytes.Equal(v.PublicKey(), current.PublicKey()) {
			continue
		}
		if err := ring.Add(v); err != nil {
			return nil, fmt.Errorf("memory: retained verifier: %w", err)
		}
	}
	return ring, nil
}

// importLegacyRecords signs every existing row as a legacy record inside the
// caller's claim transaction, which commits them with the marker.
func (s *MemoryRecordStore) importLegacyRecords(ctx context.Context, tx *sql.Tx) error {
	lastID, first := "", true
	for {
		batch, err := readLegacyBatch(ctx, tx, lastID, first)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, record := range batch {
			if record.Signature.Alg != "" || record.Signature.KeyID != "" || len(record.Signature.Bytes) != 0 {
				return errors.New("memory: signed record without initialization marker")
			}
			if err := validateRecordMeaning(record.MemoryRecordBody); err != nil {
				return fmt.Errorf("memory: invalid legacy record: %w", err)
			}
			metadata, err := normalizeMetadata(record.Metadata)
			if err != nil {
				return err
			}
			record.Metadata = []byte(metadata)
			record.Provenance.OriginTool = "legacy-migration"
			record.Provenance.OriginSessionID = record.SessionID
			record.Provenance.TrustClass = TrustLegacyUnreviewed
			if err := signRecord(ctx, s.signer, &record); err != nil {
				return fmt.Errorf("memory: sign legacy record: %w", err)
			}
			if err := persistRecord(ctx, tx, record); err != nil {
				return err
			}
		}
		lastID, first = batch[len(batch)-1].ID, false
	}
	return nil
}

func readLegacyBatch(ctx context.Context, tx *sql.Tx, lastID string, first bool) ([]MemoryRecord, error) {
	query := `SELECT ` + recordColumns + ` FROM memory_records`
	var args []any
	if !first {
		query += ` WHERE id > ?`
		args = append(args, lastID)
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY id LIMIT 128`, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: read legacy records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	batch := make([]MemoryRecord, 0, 128)
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		if err := validateRecordSize(record.MemoryRecordBody); err != nil {
			return nil, err
		}
		batch = append(batch, record)
	}
	if err := rows.Err(); err != nil {
		return nil, recordScanError(err)
	}
	if err := rows.Close(); err != nil {
		return nil, recordScanError(err)
	}
	return batch, nil
}
