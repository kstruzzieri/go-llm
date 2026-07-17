package rag

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	managedSourcePrefix              = "managed:"
	managedFailurePersistenceTimeout = 5 * time.Second
	MaxManagedDocumentBytes          = 16 << 20
	MaxManagedMetadataBytes          = 4 << 10
	MaxManagedTags                   = 64
	MaxManagedTagBytes               = 256
	MaxManagedListLimit              = 100
)

// DocumentKind identifies how a managed document's content is retained.
type DocumentKind string

const (
	DocumentKindText DocumentKind = "text"
	DocumentKindFile DocumentKind = "file"
)

// DocumentState reports the latest managed indexing outcome.
type DocumentState string

const (
	DocumentStateIndexing DocumentState = "indexing"
	DocumentStateIndexed  DocumentState = "indexed"
	DocumentStateFailed   DocumentState = "failed"
)

// DocumentFreshness reports whether indexed chunks still match their source.
type DocumentFreshness string

const (
	DocumentFreshnessUnknown DocumentFreshness = "unknown"
	DocumentFreshnessFresh   DocumentFreshness = "fresh"
	DocumentFreshnessStale   DocumentFreshness = "stale"
)

// ErrDocumentNotFound indicates that a managed document ID does not exist.
var ErrDocumentNotFound = errors.New("rag: managed document not found")

// Document is the durable metadata for a managed RAG source.
type Document struct {
	ID              string            `json:"id"`
	Title           string            `json:"title"`
	Kind            DocumentKind      `json:"kind"`
	Origin          string            `json:"origin,omitempty"`
	MIMEType        string            `json:"mime_type"`
	ContentHash     string            `json:"content_hash"`
	SourceSignature string            `json:"source_signature"`
	IndexedAt       int64             `json:"indexed_at"`
	VectorSpaceID   string            `json:"vector_space_id"`
	Collection      string            `json:"collection,omitempty"`
	Tags            []string          `json:"tags"`
	State           DocumentState     `json:"state"`
	Freshness       DocumentFreshness `json:"freshness"`
	LastError       string            `json:"last_error,omitempty"`
	ChunkCount      int               `json:"chunk_count"`
	source          string
	storedText      string
}

// DocumentOptions supplies optional metadata for a managed document.
type DocumentOptions struct {
	Title      string
	MIMEType   string
	Collection string
	Tags       []string
}

// DocumentFilter selects managed documents by exact metadata values.
type DocumentFilter struct {
	Collection string
	Tags       []string
	State      DocumentState
	Freshness  DocumentFreshness
	AfterID    string
	Limit      int
}

// ManagedSources owns managed document registry and indexing lifecycle writes.
type ManagedSources struct {
	indexer  *Indexer
	store    *SQLiteStore
	gate     managedSourceGate
	readFile func(string) ([]byte, error)
}

type managedSourceGate chan struct{}

func newManagedSourceGate() managedSourceGate {
	gate := make(managedSourceGate, 1)
	gate <- struct{}{}
	return gate
}

func (g managedSourceGate) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g:
	}
	if err := ctx.Err(); err != nil {
		g <- struct{}{}
		return err
	}
	return nil
}

func (g managedSourceGate) unlock() {
	g <- struct{}{}
}

// NewManagedSources creates a managed source service. A nil indexer leaves
// list and delete available, while ingest and reindex return an error.
func NewManagedSources(indexer *Indexer, store *SQLiteStore) (*ManagedSources, error) {
	if store == nil {
		return nil, fmt.Errorf("rag: NewManagedSources: store is required")
	}
	return &ManagedSources{indexer: indexer, store: store, gate: newManagedSourceGate(), readFile: readManagedRegularFile}, nil
}

// IngestText saves and indexes a named UTF-8 text document.
func (m *ManagedSources) IngestText(ctx context.Context, name, content string, opts DocumentOptions) (Document, error) {
	if err := m.gate.lock(ctx); err != nil {
		return Document{}, err
	}
	defer m.gate.unlock()
	if m.indexer == nil {
		return Document{}, fmt.Errorf("rag: managed sources: indexer is unavailable")
	}
	if !utf8.ValidString(name) || !utf8.ValidString(content) {
		return Document{}, fmt.Errorf("rag: ingest text: input must be valid UTF-8")
	}
	if len(name) > MaxManagedMetadataBytes || len(content) > MaxManagedDocumentBytes {
		return Document{}, fmt.Errorf("rag: ingest text: input exceeds managed size limit")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Document{}, fmt.Errorf("rag: ingest text: name is required")
	}
	if strings.TrimSpace(content) == "" {
		return Document{}, fmt.Errorf("rag: ingest text %q: content is required", name)
	}
	return m.ingestLocked(ctx, name, content, DocumentKindText, "", content, opts)
}

// IngestFile reads and indexes a local UTF-8 file by canonical path.
func (m *ManagedSources) IngestFile(ctx context.Context, path string, opts DocumentOptions) (Document, error) {
	if err := m.gate.lock(ctx); err != nil {
		return Document{}, err
	}
	defer m.gate.unlock()
	if m.indexer == nil {
		return Document{}, fmt.Errorf("rag: managed sources: indexer is unavailable")
	}
	if strings.TrimSpace(path) == "" {
		return Document{}, fmt.Errorf("rag: ingest file: path is required")
	}
	if !utf8.ValidString(path) || len(path) > MaxManagedMetadataBytes {
		return Document{}, fmt.Errorf("rag: ingest file: path exceeds managed size limit or is not valid UTF-8")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Document{}, fmt.Errorf("rag: ingest file %q: resolve path: %w", path, err)
	}
	abs = filepath.Clean(abs)
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	data, err := m.readFile(abs)
	if err != nil {
		return Document{}, fmt.Errorf("rag: ingest file %q: %w", abs, err)
	}
	if !utf8.Valid(data) {
		return Document{}, fmt.Errorf("rag: ingest file %q: content must be valid UTF-8", abs)
	}
	return m.ingestLocked(ctx, filepath.Base(abs), string(data), DocumentKindFile, abs, "", opts)
}

func (m *ManagedSources) ingestLocked(ctx context.Context, name, content string, kind DocumentKind, origin, storedText string, opts DocumentOptions) (Document, error) {
	var err error
	opts, err = normalizeManagedDocumentOptions(name, opts)
	if err != nil {
		return Document{}, err
	}
	id, err := newManagedDocumentID()
	if err != nil {
		return Document{}, err
	}
	title := opts.Title
	if title == "" {
		title = name
	}
	document := Document{
		ID:              id,
		Title:           title,
		Kind:            kind,
		Origin:          origin,
		MIMEType:        opts.MIMEType,
		ContentHash:     contentHash(content),
		SourceSignature: m.indexer.currentSourceSignature(content).String(),
		Collection:      opts.Collection,
		Tags:            opts.Tags,
		State:           DocumentStateIndexing,
		Freshness:       DocumentFreshnessUnknown,
		source:          managedSourcePrefix + id + strings.ToLower(filepath.Ext(name)),
		storedText:      storedText,
	}
	now := time.Now().Unix()
	tags, _ := json.Marshal(document.Tags)
	_, err = m.store.db.ExecContext(ctx, `
		INSERT INTO managed_documents (
			id, source, title, kind, origin, mime_type, stored_text,
			content_hash, source_signature, indexed_at, vector_space_id,
			collection, tags, state, freshness, last_error, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?, 'indexing', 'unknown', '', ?, ?)`,
		document.ID, document.source, document.Title, document.Kind, document.Origin,
		document.MIMEType, document.storedText, document.ContentHash,
		document.SourceSignature, document.Collection, string(tags), now, now)
	if err != nil {
		return document, fmt.Errorf("rag: register managed document %q: %w", document.ID, err)
	}
	return m.indexDocumentLocked(ctx, document, content, true)
}

// ListDocuments returns reconciled managed documents ordered by ID.
func (m *ManagedSources) ListDocuments(ctx context.Context, filter DocumentFilter) ([]Document, error) {
	if err := m.gate.lock(ctx); err != nil {
		return nil, err
	}
	defer m.gate.unlock()
	if filter.Limit <= 0 {
		filter.Limit = MaxManagedListLimit
	}
	if filter.Limit > MaxManagedListLimit {
		return nil, fmt.Errorf("rag: list managed documents: limit exceeds %d", MaxManagedListLimit)
	}
	if !utf8.ValidString(filter.AfterID) || len(filter.AfterID) > MaxManagedMetadataBytes ||
		!utf8.ValidString(filter.Collection) || len(filter.Collection) > MaxManagedMetadataBytes {
		return nil, fmt.Errorf("rag: list managed documents: filter exceeds managed size limit or is not valid UTF-8")
	}
	wantedTags, err := normalizeManagedTags(filter.Tags)
	if err != nil {
		return nil, err
	}
	filtered := make([]Document, 0, filter.Limit)
	afterID := filter.AfterID
	for len(filtered) < filter.Limit {
		rows, err := m.store.db.QueryContext(ctx, managedDocumentListSelect+` WHERE id > ? ORDER BY id LIMIT ?`, afterID, MaxManagedListLimit)
		if err != nil {
			return nil, fmt.Errorf("rag: list managed documents: %w", err)
		}
		batch := make([]Document, 0, MaxManagedListLimit)
		for rows.Next() {
			document, err := scanManagedDocument(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			batch = append(batch, document)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("rag: iterate managed documents: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("rag: close managed documents: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		afterID = batch[len(batch)-1].ID
		for i := range batch {
			document := &batch[i]
			if document.State == DocumentStateIndexed {
				if _, err := m.reconcileDocument(ctx, document); err != nil {
					return nil, err
				}
			}
			if filter.Collection != "" && document.Collection != filter.Collection ||
				filter.State != "" && document.State != filter.State ||
				filter.Freshness != "" && document.Freshness != filter.Freshness ||
				!containsManagedTags(document.Tags, wantedTags) {
				continue
			}
			filtered = append(filtered, *document)
			if len(filtered) == filter.Limit {
				break
			}
		}
		if len(batch) < MaxManagedListLimit {
			break
		}
	}
	return filtered, nil
}

// reconcileDocument revalidates a listing snapshot before changing its
// lifecycle state. A separate ManagedSources instance may finish a reindex
// between ListDocuments' read and reconciliation write.
func (m *ManagedSources) reconcileDocument(ctx context.Context, observed *Document) (bool, error) {
	desiredFreshness := DocumentFreshnessFresh
	if observed.Kind == DocumentKindFile {
		data, err := m.readFile(observed.Origin)
		if err != nil || !utf8.Valid(data) || contentHash(string(data)) != observed.ContentHash {
			desiredFreshness = DocumentFreshnessStale
		}
	}

	tx, err := m.store.beginWriteTx(ctx)
	if err != nil {
		return false, fmt.Errorf("rag: reconcile managed document %q: begin transaction: %w", observed.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := scanManagedDocument(tx.QueryRowContext(ctx, managedDocumentListSelect+` WHERE id = ?`, observed.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameManagedDocumentRevision(*observed, current) {
		*observed = current
		return false, nil
	}

	var chunks int
	var minSignature, maxSignature, minVectorSpaceID, maxVectorSpaceID, minDocumentID, maxDocumentID string
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(MIN(source_content_hash), ''), COALESCE(MAX(source_content_hash), ''),
		       COALESCE(MIN(vector_space_id), ''), COALESCE(MAX(vector_space_id), ''),
		       COALESCE(MIN(json_extract(metadata, '$.managed_document_id')), ''),
		       COALESCE(MAX(json_extract(metadata, '$.managed_document_id')), '')
		  FROM chunks
		 WHERE source = ?`, current.source).Scan(
		&chunks, &minSignature, &maxSignature, &minVectorSpaceID, &maxVectorSpaceID,
		&minDocumentID, &maxDocumentID,
	); err != nil {
		return false, fmt.Errorf("rag: count chunks for managed document %q: %w", current.ID, err)
	}
	provenanceMismatch := chunks > 0 &&
		(minSignature != current.SourceSignature || maxSignature != current.SourceSignature ||
			minVectorSpaceID != current.VectorSpaceID || maxVectorSpaceID != current.VectorSpaceID ||
			minDocumentID != current.ID || maxDocumentID != current.ID)
	state, freshness, lastError := current.State, current.Freshness, current.LastError
	if chunks != current.ChunkCount || provenanceMismatch {
		state, freshness, lastError = DocumentStateFailed, DocumentFreshnessStale, "managed document chunks are missing or inconsistent"
	} else {
		freshness = desiredFreshness
	}
	if state == current.State && freshness == current.Freshness && lastError == current.LastError {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE managed_documents
		   SET state = ?, freshness = ?, last_error = ?, updated_at = ?
		 WHERE id = ?`, state, freshness, lastError, time.Now().Unix(), current.ID); err != nil {
		return false, fmt.Errorf("rag: reconcile managed document %q status: %w", current.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE chunks
		   SET metadata = json_set(metadata,
		       '$.managed_state', ?, '$.managed_freshness', ?)
		 WHERE source = ?`, string(state), string(freshness), current.source); err != nil {
		return false, fmt.Errorf("rag: reconcile chunks for managed document %q status: %w", current.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("rag: reconcile managed document %q status: commit: %w", current.ID, err)
	}
	current.State, current.Freshness, current.LastError = state, freshness, lastError
	*observed = current
	return true, nil
}

func sameManagedDocumentRevision(left, right Document) bool {
	if left.ID != right.ID || left.source != right.source || left.Title != right.Title || left.Kind != right.Kind ||
		left.Origin != right.Origin || left.MIMEType != right.MIMEType || left.ContentHash != right.ContentHash ||
		left.SourceSignature != right.SourceSignature || left.IndexedAt != right.IndexedAt ||
		left.VectorSpaceID != right.VectorSpaceID || left.Collection != right.Collection ||
		left.State != right.State || left.Freshness != right.Freshness || left.LastError != right.LastError ||
		left.ChunkCount != right.ChunkCount || len(left.Tags) != len(right.Tags) {
		return false
	}
	for i := range left.Tags {
		if left.Tags[i] != right.Tags[i] {
			return false
		}
	}
	return true
}

// DeleteDocument atomically removes a managed document and its chunks.
func (m *ManagedSources) DeleteDocument(ctx context.Context, id string) error {
	if err := m.gate.lock(ctx); err != nil {
		return err
	}
	defer m.gate.unlock()
	tx, err := m.store.beginWriteTx(ctx)
	if err != nil {
		return fmt.Errorf("rag: delete managed document %q: begin transaction: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var source string
	if err := tx.QueryRowContext(ctx, `SELECT source FROM managed_documents WHERE id = ?`, id).Scan(&source); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrDocumentNotFound, id)
		}
		return fmt.Errorf("rag: load managed document %q for delete: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE source = ?`, source); err != nil {
		return fmt.Errorf("rag: delete chunks for managed document %q: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM managed_documents WHERE id = ?`, id); err != nil {
		return fmt.Errorf("rag: delete managed document %q: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: delete managed document %q: commit: %w", id, err)
	}
	return nil
}

// ReindexDocument rereads retained content and atomically replaces its chunks.
func (m *ManagedSources) ReindexDocument(ctx context.Context, id string) (Document, error) {
	if err := m.gate.lock(ctx); err != nil {
		return Document{}, err
	}
	defer m.gate.unlock()
	if m.indexer == nil {
		return Document{}, fmt.Errorf("rag: managed sources: indexer is unavailable")
	}
	document, err := m.loadDocument(ctx, id)
	if err != nil {
		return Document{}, err
	}
	content := document.storedText
	if document.Kind == DocumentKindFile {
		data, readErr := m.readFile(document.Origin)
		if readErr != nil {
			cause := fmt.Errorf("rag: read managed file %q: %w", document.Origin, readErr)
			return m.failedDocument(ctx, document, false, cause)
		}
		if !utf8.Valid(data) {
			cause := fmt.Errorf("rag: read managed file %q: content must be valid UTF-8", document.Origin)
			return m.failedDocument(ctx, document, false, cause)
		}
		content = string(data)
	}
	return m.indexDocumentLocked(ctx, document, content, false)
}

func (m *ManagedSources) indexDocumentLocked(ctx context.Context, document Document, content string, first bool) (Document, error) {
	if !utf8.ValidString(content) || len(content) > MaxManagedDocumentBytes {
		return m.failedDocument(ctx, document, first, fmt.Errorf("rag: managed document content exceeds %d-byte limit or is not valid UTF-8", MaxManagedDocumentBytes))
	}
	candidate := document
	candidate.ContentHash = contentHash(content)
	metadata := managedDocumentMetadata(candidate)
	prepared, err := m.indexer.prepareSource(ctx, document.source, content, metadata)
	if err != nil {
		return m.failedDocument(ctx, document, first, err)
	}
	indexedAt := time.Now().Unix()
	vectorSpaceID, err := m.commitDocumentIndex(ctx, candidate, prepared, indexedAt)
	if err != nil {
		return m.failedDocument(ctx, document, first, err)
	}
	candidate.SourceSignature = prepared.sourceHash
	candidate.IndexedAt = indexedAt
	candidate.VectorSpaceID = vectorSpaceID
	candidate.ChunkCount = len(prepared.chunks)
	candidate.State = DocumentStateIndexed
	candidate.Freshness = DocumentFreshnessFresh
	candidate.LastError = ""
	return candidate, nil
}

func (m *ManagedSources) commitDocumentIndex(ctx context.Context, document Document, prepared preparedSource, indexedAt int64) (string, error) {
	opts := replaceSourceOptions{
		sourceHash:               prepared.sourceHash,
		vectorSpaceID:            prepared.vectorSpaceID,
		requireVectorSpaceID:     true,
		checkExistingVectorSpace: false,
		replaceVectorSpace:       true,
	}
	if err := m.store.validateReplaceSource(document.source, prepared.chunks, prepared.embeddings, opts); err != nil {
		return "", err
	}
	tx, err := m.store.beginWriteTx(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: finalize managed document %q: begin transaction: %w", ErrStoreOperation, document.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var retainedVectorSpaceID string
	if err := tx.QueryRowContext(ctx, `SELECT vector_space_id FROM managed_documents WHERE id = ?`, document.ID).Scan(&retainedVectorSpaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrDocumentNotFound, document.ID)
		}
		return "", fmt.Errorf("rag: load managed document %q vector space: %w", document.ID, err)
	}
	if len(prepared.chunks) > 0 {
		if err := validateCorpusVectorSpaceTx(ctx, tx, document.source, prepared.vectorSpaceID); err != nil {
			return "", err
		}
	}
	vectorSpaceID := prepared.vectorSpaceID
	if len(prepared.chunks) == 0 {
		vectorSpaceID = retainedVectorSpaceID
	}
	opts.vectorSpaceID = vectorSpaceID
	if err := m.store.replaceSourceTx(ctx, tx, document.source, prepared.chunks, prepared.embeddings, opts); err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE managed_documents
		   SET content_hash = ?, source_signature = ?, indexed_at = ?, vector_space_id = ?, chunk_count = ?,
		       state = 'indexed', freshness = 'fresh', last_error = '', updated_at = ?
		 WHERE id = ?`, document.ContentHash, prepared.sourceHash, indexedAt,
		vectorSpaceID, len(prepared.chunks), indexedAt, document.ID)
	if err != nil {
		return "", fmt.Errorf("rag: finalize managed document %q: %w", document.ID, err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return "", fmt.Errorf("rag: finalize managed document %q: rows affected: %w", document.ID, err)
		}
		return "", fmt.Errorf("%w: %s", ErrDocumentNotFound, document.ID)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("rag: finalize managed document %q: commit: %w", document.ID, err)
	}
	return vectorSpaceID, nil
}

func (m *ManagedSources) failedDocument(ctx context.Context, document Document, first bool, cause error) (Document, error) {
	freshness := DocumentFreshnessStale
	if first {
		freshness = DocumentFreshnessUnknown
	}
	document.State = DocumentStateFailed
	document.Freshness = freshness
	document.LastError = cause.Error()
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), managedFailurePersistenceTimeout)
	defer cancel()
	if err := m.persistStatus(persistCtx, &document, document.State, document.Freshness, document.LastError); err != nil {
		return document, errors.Join(cause, fmt.Errorf("rag: record managed document failure: %w", err))
	}
	return document, cause
}

func (m *ManagedSources) persistStatus(ctx context.Context, document *Document, state DocumentState, freshness DocumentFreshness, lastError string) error {
	tx, err := m.store.beginWriteTx(ctx)
	if err != nil {
		return fmt.Errorf("rag: update managed document %q status: begin transaction: %w", document.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE managed_documents
		   SET state = ?, freshness = ?, last_error = ?, updated_at = ?
		 WHERE id = ?`, state, freshness, lastError, time.Now().Unix(), document.ID)
	if err != nil {
		return fmt.Errorf("rag: update managed document %q status: %w", document.ID, err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return fmt.Errorf("rag: update managed document %q status: rows affected: %w", document.ID, err)
		}
		return fmt.Errorf("%w: %s", ErrDocumentNotFound, document.ID)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE chunks
		   SET metadata = json_set(metadata,
		       '$.managed_state', ?, '$.managed_freshness', ?)
		 WHERE source = ?`, string(state), string(freshness), document.source); err != nil {
		return fmt.Errorf("rag: update chunks for managed document %q status: %w", document.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: update managed document %q status: commit: %w", document.ID, err)
	}
	document.State = state
	document.Freshness = freshness
	document.LastError = lastError
	return nil
}

const managedDocumentSelect = `
	SELECT id, source, title, kind, origin, mime_type, stored_text,
	       content_hash, source_signature, indexed_at, vector_space_id,
	       chunk_count, collection, tags, state, freshness, last_error
	  FROM managed_documents`

// managedDocumentListSelect mirrors managedDocumentSelect but skips loading
// stored_text: retained bodies can be large and listing never needs them.
const managedDocumentListSelect = `
	SELECT id, source, title, kind, origin, mime_type, '' AS stored_text,
	       content_hash, source_signature, indexed_at, vector_space_id,
	       chunk_count, collection, tags, state, freshness, last_error
	  FROM managed_documents`

type managedRowScanner interface {
	Scan(dest ...any) error
}

func scanManagedDocument(scanner managedRowScanner) (Document, error) {
	var document Document
	var tagsJSON string
	if err := scanner.Scan(
		&document.ID, &document.source, &document.Title, &document.Kind,
		&document.Origin, &document.MIMEType, &document.storedText,
		&document.ContentHash, &document.SourceSignature, &document.IndexedAt,
		&document.VectorSpaceID, &document.ChunkCount, &document.Collection, &tagsJSON,
		&document.State, &document.Freshness, &document.LastError,
	); err != nil {
		return Document{}, fmt.Errorf("rag: scan managed document: %w", err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &document.Tags); err != nil {
		return Document{}, fmt.Errorf("rag: decode tags for managed document %q: %w", document.ID, err)
	}
	if document.Tags == nil {
		document.Tags = []string{}
	}
	return document, nil
}

func (m *ManagedSources) loadDocument(ctx context.Context, id string) (Document, error) {
	document, err := scanManagedDocument(m.store.db.QueryRowContext(ctx, managedDocumentSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, fmt.Errorf("%w: %s", ErrDocumentNotFound, id)
	}
	if err != nil {
		return Document{}, err
	}
	return document, nil
}

func managedDocumentMetadata(document Document) map[string]string {
	tags, _ := json.Marshal(document.Tags)
	return map[string]string{
		"managed_document_id":  document.ID,
		"managed_title":        document.Title,
		"managed_kind":         string(document.Kind),
		"managed_origin":       document.Origin,
		"managed_mime_type":    document.MIMEType,
		"managed_content_hash": document.ContentHash,
		"managed_collection":   document.Collection,
		"managed_tags":         string(tags),
		"managed_state":        string(DocumentStateIndexed),
		"managed_freshness":    string(DocumentFreshnessFresh),
	}
}

func readManagedRegularFile(path string) ([]byte, error) {
	f, err := openManagedFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxManagedDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManagedDocumentBytes {
		return nil, fmt.Errorf("document exceeds %d-byte limit", MaxManagedDocumentBytes)
	}
	return data, nil
}

func newManagedDocumentID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("rag: generate managed document id: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func managedMIMEType(name, explicit string) string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit
	}
	if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); detected != "" {
		return detected
	}
	return "application/octet-stream"
}

func normalizeManagedDocumentOptions(name string, opts DocumentOptions) (DocumentOptions, error) {
	for _, field := range []string{opts.Title, opts.MIMEType, opts.Collection} {
		if !utf8.ValidString(field) || len(field) > MaxManagedMetadataBytes {
			return DocumentOptions{}, fmt.Errorf("rag: managed metadata exceeds %d-byte limit or is not valid UTF-8", MaxManagedMetadataBytes)
		}
	}
	opts.Title = strings.TrimSpace(opts.Title)
	if opts.Title == "" {
		opts.Title = name
	}
	opts.MIMEType = managedMIMEType(name, opts.MIMEType)
	opts.Collection = strings.TrimSpace(opts.Collection)
	for _, field := range []string{opts.Title, opts.MIMEType, opts.Collection} {
		if !utf8.ValidString(field) || len(field) > MaxManagedMetadataBytes {
			return DocumentOptions{}, fmt.Errorf("rag: managed metadata exceeds %d-byte limit or is not valid UTF-8", MaxManagedMetadataBytes)
		}
	}
	var err error
	opts.Tags, err = normalizeManagedTags(opts.Tags)
	if err != nil {
		return DocumentOptions{}, err
	}
	return opts, nil
}

func normalizeManagedTags(tags []string) ([]string, error) {
	if len(tags) > MaxManagedTags {
		return nil, fmt.Errorf("rag: managed tags exceed %d-tag limit", MaxManagedTags)
	}
	seen := make(map[string]struct{}, MaxManagedTags)
	for _, tag := range tags {
		if !utf8.ValidString(tag) {
			return nil, fmt.Errorf("rag: managed tag must be valid UTF-8")
		}
		if len(tag) > MaxManagedTagBytes {
			return nil, fmt.Errorf("rag: managed tag exceeds %d-byte limit", MaxManagedTagBytes)
		}
		if tag = strings.TrimSpace(tag); tag != "" {
			seen[tag] = struct{}{}
		}
	}
	normalized := make([]string, 0, len(seen))
	for tag := range seen {
		normalized = append(normalized, tag)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func containsManagedTags(have, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, tag := range have {
		set[tag] = struct{}{}
	}
	for _, tag := range wanted {
		if _, ok := set[tag]; !ok {
			return false
		}
	}
	return true
}

func (s *SQLiteStore) hasManagedDocumentSource(ctx context.Context, source string) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM managed_documents WHERE source = ?)`,
		source,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("rag: check managed source %q: %w", source, err)
	}
	return exists, nil
}
