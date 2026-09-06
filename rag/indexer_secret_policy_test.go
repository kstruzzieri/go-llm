package rag

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func policyTestSecret() string { return "sk-" + "aB3_dE7-fG9_hJ2-kL4" }

func TestIndexPolicyErrorContract(t *testing.T) {
	secret := policyTestSecret()
	cause := errors.New("provider rejected " + secret)
	for _, unsafe := range []bool{false, true} {
		policy := &IndexPolicyError{Outcome: IndexPolicyOutcome{
			Path: "dir/" + secret + ".md", Action: IndexPolicySkip,
			Kinds: []SensitiveKind{SensitiveOpenAIToken}, Unsafe: unsafe,
		}}
		if unsafe {
			policy.Err = cause
		}
		wrapped := fmt.Errorf("index file: %w", policy)
		joined := errors.Join(errors.New("other failure"), wrapped)
		var got *IndexPolicyError
		if !errors.As(joined, &got) || got != policy {
			t.Fatal("joined per-file error lost policy identity")
		}
		if unsafe && !errors.Is(joined, cause) {
			t.Fatal("joined policy failure lost underlying cause")
		}
		text := wrapped.Error()
		if strings.Contains(text, secret) || strings.Contains(text, "provider rejected") {
			t.Fatal("policy diagnostic disclosed path content or cause text")
		}
		if !strings.Contains(text, "[REDACTED_SECRET]") || !strings.Contains(text, "openai_token") {
			t.Fatal("policy diagnostic omitted safe path/kind metadata")
		}
	}
}

type emptyIndexErrorJoin struct{}

func (emptyIndexErrorJoin) Error() string   { return "empty" }
func (emptyIndexErrorJoin) Unwrap() []error { return nil }

func TestIsSafeIndexSkipCompleteTree(t *testing.T) {
	safe := &IndexPolicyError{Outcome: IndexPolicyOutcome{Action: IndexPolicySkip}}
	unsafe := &IndexPolicyError{Outcome: IndexPolicyOutcome{Action: IndexPolicySkip, Unsafe: true}}
	redact := &IndexPolicyError{Outcome: IndexPolicyOutcome{Action: IndexPolicyRedact}}
	ordinary := errors.New("ordinary")
	var nilPolicy *IndexPolicyError
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false}, {"typed nil", nilPolicy, false}, {"empty join", emptyIndexErrorJoin{}, false},
		{"safe", safe, true}, {"wrapped", fmt.Errorf("file: %w", safe), true},
		{"safe join", errors.Join(safe, fmt.Errorf("file: %w", safe)), true},
		{"nested join", fmt.Errorf("directory: %w", errors.Join(safe, errors.Join(safe, safe))), true},
		{"unsafe", unsafe, false}, {"redact", redact, false}, {"ordinary", ordinary, false},
		{"ordinary last", errors.Join(safe, ordinary), false}, {"ordinary first", errors.Join(ordinary, safe), false},
		{"unsafe last", errors.Join(safe, unsafe), false}, {"unsafe first", errors.Join(unsafe, safe), false},
		{"multi wrap", fmt.Errorf("%w and %w", safe, ordinary), false},
		{"skip with cause", &IndexPolicyError{Outcome: safe.Outcome, Err: ordinary}, false},
		{"skip with safe cause", &IndexPolicyError{Outcome: safe.Outcome, Err: safe}, false},
		{"unsafe with safe cause", &IndexPolicyError{Outcome: unsafe.Outcome, Err: safe}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSafeIndexSkip(tc.err); got != tc.want {
				t.Fatalf("IsSafeIndexSkip = %t, want %t", got, tc.want)
			}
		})
	}
}

func policyTestIndexer(t *testing.T, opts ...IndexerOption) (*Indexer, *SQLiteStore, *atomic.Int32) {
	t.Helper()
	store := newTestStore(t)
	var calls atomic.Int32
	embedder := EmbedderFunc(func(ctx context.Context, model string, inputs []string) (EmbedResult, error) {
		calls.Add(1)
		return stubEmbedder("p/m").Embed(ctx, model, inputs)
	})
	idx, err := NewIndexerWithEmbedder(embedder, store, opts...)
	if err != nil {
		t.Fatal("construct policy test indexer")
	}
	return idx, store, &calls
}

func seedPolicySource(t *testing.T, idx *Indexer, store *SQLiteStore, path, content string) {
	t.Helper()
	chunk := makeChunk(path, content, 1, 1, "")
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(context.Background(), path,
		[]Chunk{chunk}, [][]float64{{1, 0, 0}}, idx.currentSourceSignature(content).String(), "p/m"); err != nil {
		t.Fatal("seed policy source")
	}
	row := validSummary()
	row.Source = path
	if err := store.UpsertSourceSummary(context.Background(), row); err != nil {
		t.Fatal("seed policy source summary")
	}
}

func requirePolicySourceAbsent(t *testing.T, store *SQLiteStore, path string) {
	t.Helper()
	chunks, err := store.GetBySource(context.Background(), path)
	if err != nil || len(chunks) != 0 {
		t.Fatal("policy source still available or unreadable")
	}
	if summaryStored(t, store, path) {
		t.Fatal("policy source summary remains available")
	}
}

func TestIndexFilePolicySkipClearsSource(t *testing.T) {
	idx, store, calls := policyTestIndexer(t)
	path := filepath.Join(t.TempDir(), "secret.md")
	content := "credential: " + policyTestSecret()
	writeFile(t, path, content)
	seedPolicySource(t, idx, store, path, content)
	var chunkCalls atomic.Int32
	idx.chunker = &countingChunker{inner: NewCodeChunker(), counter: &chunkCalls}
	err := idx.IndexFile(context.Background(), path)
	if !IsSafeIndexSkip(err) {
		t.Fatal("IndexFile must report a safely cleared policy skip")
	}
	var policy *IndexPolicyError
	if !errors.As(err, &policy) || policy.Outcome.Path != path || len(policy.Outcome.Kinds) != 1 || policy.Outcome.Kinds[0] != SensitiveOpenAIToken {
		t.Fatal("IndexFile skip lost source/kind metadata")
	}
	if calls.Load() != 0 || chunkCalls.Load() != 0 {
		t.Fatal("skipped content reached chunking or embedding")
	}
	requirePolicySourceAbsent(t, store, path)
}

func TestIndexFilePolicyRedactionAndStatus(t *testing.T) {
	secret := policyTestSecret()
	card := "45320198" + "7654321" + "5"
	for _, tc := range []struct {
		name, content, want string
		kinds               []SensitiveKind
		wantKinds           []SensitiveKind
		wantSkip            bool
	}{
		{"provider", secret, "[REDACTED_SECRET]", []SensitiveKind{SensitiveOpenAIToken}, []SensitiveKind{SensitiveOpenAIToken}, false},
		{"canonical union", `token="before!` + secret + `!after"`, `token="[REDACTED_SECRET]"`, []SensitiveKind{SensitiveOpenAIToken}, []SensitiveKind{SensitiveOpenAIToken}, false},
		{"discarded kind cannot authorize", `token="` + secret + `"`, "", []SensitiveKind{SensitiveSecretAssignment}, []SensitiveKind{SensitiveOpenAIToken}, true},
		{"distinct group skip", secret + "\n" + card, "", []SensitiveKind{SensitiveOpenAIToken}, []SensitiveKind{SensitiveOpenAIToken, SensitivePaymentCard}, true},
		{"both groups redact", secret + "\n" + card, "[REDACTED_SECRET]\n[REDACTED_PAYMENT_CARD]", []SensitiveKind{SensitivePaymentCard, SensitiveOpenAIToken}, []SensitiveKind{SensitiveOpenAIToken, SensitivePaymentCard}, false},
		{"unknown option fails closed", secret, "", []SensitiveKind{"unknown"}, []SensitiveKind{SensitiveOpenAIToken}, true},
		{"deduplicated kinds", secret + "\n" + secret, "[REDACTED_SECRET]\n[REDACTED_SECRET]", []SensitiveKind{SensitiveOpenAIToken}, []SensitiveKind{SensitiveOpenAIToken}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, store, calls := policyTestIndexer(t, WithSensitiveRedaction(tc.kinds...))
			path := filepath.Join(t.TempDir(), "source.md")
			writeFile(t, path, tc.content)
			seedPolicySource(t, idx, store, path, tc.content)
			var chunked, embedded string
			idx.chunker = chunkerFunc(func(source, content string) ([]Chunk, error) {
				chunked = content
				return []Chunk{makeChunk(source, content, 1, 1, "")}, nil
			})
			previous := idx.embedder
			idx.embedder = EmbedderFunc(func(ctx context.Context, model string, inputs []string) (EmbedResult, error) {
				embedded = strings.Join(inputs, "\n")
				return previous.Embed(ctx, model, inputs)
			})
			status, err := idx.IndexFileWithStatus(context.Background(), path)
			if status.TotalFiles != 1 || status.SkippedFiles != 0 || len(status.Errors) != 0 || status.InProgress || len(status.PolicyOutcomes) != 1 {
				t.Fatal("file status lost policy outcome or counted it as cancellation/failure")
			}
			outcome := status.PolicyOutcomes[0]
			if outcome.Path != path || outcome.Unsafe || !reflect.DeepEqual(outcome.Kinds, tc.wantKinds) {
				t.Fatal("file status has incorrect policy metadata")
			}
			if tc.wantSkip {
				if !IsSafeIndexSkip(err) || outcome.Action != IndexPolicySkip || status.IndexedFiles != 0 || calls.Load() != 0 || chunked != "" {
					t.Fatal("file-wide skip failed")
				}
				requirePolicySourceAbsent(t, store, path)
				return
			}
			if err != nil || outcome.Action != IndexPolicyRedact || status.IndexedFiles != 1 {
				t.Fatal("successful redaction must have nil error and indexed Redact status")
			}
			chunks, loadErr := store.GetBySource(context.Background(), path)
			if loadErr != nil || len(chunks) != 1 || chunks[0].Chunk.Content != tc.want || chunked != tc.want || embedded != tc.want {
				t.Fatal("raw or incorrectly redacted content reached chunker/embedder/store")
			}
			if summaryStored(t, store, path) {
				t.Fatal("redaction left the old source summary available")
			}
		})
	}
}

func TestIndexFilePolicyOptionsOwnAndComposeKinds(t *testing.T) {
	kinds := []SensitiveKind{SensitiveOpenAIToken}
	option := WithSensitiveRedaction(kinds...)
	kinds[0] = SensitivePrivateKey
	idx, _, _ := policyTestIndexer(t, option, WithSensitiveRedaction(SensitivePaymentCard, SensitivePaymentCard))
	path := filepath.Join(t.TempDir(), "source.md")
	writeFile(t, path, policyTestSecret()+"\n"+"45320198"+"7654321"+"5")
	status, err := idx.IndexFileWithStatus(context.Background(), path)
	if err != nil || len(status.PolicyOutcomes) != 1 || status.PolicyOutcomes[0].Action != IndexPolicyRedact {
		t.Fatal("redaction options did not own input or compose selected kinds")
	}
}

func TestIndexFileIncrementalPolicyBeforeHash(t *testing.T) {
	idx, store, calls := policyTestIndexer(t)
	path := filepath.Join(t.TempDir(), "source.md")
	content := policyTestSecret()
	writeFile(t, path, content)
	seedPolicySource(t, idx, store, path, content)
	err := idx.IndexFileIncremental(context.Background(), path)
	if !IsSafeIndexSkip(err) {
		t.Fatal("raw matching source hash bypassed default policy scan")
	}
	if calls.Load() != 0 {
		t.Fatal("incremental policy skip embedded content")
	}
	requirePolicySourceAbsent(t, store, path)
}

type policyTestStore struct {
	*SQLiteStore
	clearErr, publishErr error
	listErr              error
	failPath             string
	afterClear           func()
	clears, publishes    atomic.Int32
	loads, cas           atomic.Int32
}

func (s *policyTestStore) ReplaceSourceWithHashAndVectorSpaceID(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64, hash, vsid string) error {
	if len(chunks) == 0 {
		s.clears.Add(1)
		if s.clearErr != nil && (s.failPath == "" || s.failPath == path) {
			return s.clearErr
		}
	} else {
		s.publishes.Add(1)
		if s.publishErr != nil && (s.failPath == "" || s.failPath == path) {
			return s.publishErr
		}
	}
	err := s.SQLiteStore.ReplaceSourceWithHashAndVectorSpaceID(ctx, path, chunks, embeddings, hash, vsid)
	if err == nil && len(chunks) == 0 && s.afterClear != nil {
		s.afterClear()
	}
	return err
}

func (s *policyTestStore) GetBySource(ctx context.Context, source string) ([]ChunkWithEmbedding, error) {
	s.loads.Add(1)
	return s.SQLiteStore.GetBySource(ctx, source)
}

func (s *policyTestStore) ListSources(ctx context.Context) ([]string, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.SQLiteStore.ListSources(ctx)
}

func (s *policyTestStore) ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64, hash, vsid, expected string) error {
	s.cas.Add(1)
	return s.SQLiteStore.ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash(ctx, path, chunks, embeddings, hash, vsid, expected)
}

func TestIndexFileIncrementalPolicySanitizedSnapshot(t *testing.T) {
	idx, inner, _ := policyTestIndexer(t, WithSensitiveRedaction(SensitiveOpenAIToken))
	store := &policyTestStore{SQLiteStore: inner}
	idx.store = store
	root := t.TempDir()
	idx.workspaceRoot = root
	path := filepath.Join(root, "source.go")
	raw := policyTestSecret() + "\nunchanged safe content"
	want := "[REDACTED_SECRET]\nunchanged safe content"
	var chunkCalls atomic.Int32
	idx.chunker = chunkerFunc(func(source, content string) ([]Chunk, error) {
		chunkCalls.Add(1)
		parts := strings.SplitN(content, "\n", 2)
		chunks := make([]Chunk, len(parts))
		for i, part := range parts {
			chunks[i] = makeChunk(source, part, i+1, i+1, "go")
			chunks[i].Metadata = map[string]string{"symbol_path": fmt.Sprintf("F%d", i), "chunk_ordinal": "0"}
		}
		return chunks, nil
	})
	oldChunks, _ := idx.chunker.Chunk(path, raw)
	populateFixtureStableKeys(t, oldChunks, root)
	if err := inner.ReplaceSourceWithHashAndVectorSpaceID(context.Background(), path, oldChunks,
		[][]float64{{1, 0, 0}, {0, 1, 0}}, idx.currentSourceSignature(raw).String(), "p/m"); err != nil {
		t.Fatal("seed incremental policy source")
	}
	row := validSummary()
	row.Source = path
	if err := inner.UpsertSourceSummary(context.Background(), row); err != nil {
		t.Fatal("seed incremental summary")
	}
	chunkCalls.Store(0)
	var embedCalls, embeddedCount int
	var sawUncleared, sawRaw bool
	idx.embedder = EmbedderFunc(func(ctx context.Context, model string, inputs []string) (EmbedResult, error) {
		embedCalls++
		embeddedCount += len(inputs)
		chunks, err := inner.GetBySource(ctx, path)
		summaries, summaryErr := inner.SourceSummaryBatch(ctx, []string{path})
		sawUncleared = err != nil || summaryErr != nil || len(chunks) != 0 || len(summaries) != 0
		sawRaw = strings.Join(inputs, "\n") != want
		// A changed disk file must not replace the already-scanned snapshot.
		writeFile(t, path, "new disk content")
		return stubEmbedder("p/m").Embed(ctx, model, inputs)
	})
	writeFile(t, path, raw)
	if err := idx.IndexFileIncremental(context.Background(), path); err != nil {
		t.Fatal("changed incremental redaction failed")
	}
	if embedCalls != 1 || embeddedCount != 2 || sawRaw || sawUncleared || store.loads.Load() != 0 || store.cas.Load() != 0 || store.clears.Load() != 1 {
		t.Fatal("changed redaction reused raw hash/cache/CAS or failed to clear before embedding")
	}
	chunks, err := inner.GetBySource(context.Background(), path)
	if err != nil || len(chunks) != 2 || chunks[0].Chunk.Content+"\n"+chunks[1].Chunk.Content != want {
		t.Fatal("replacement did not publish the original sanitized snapshot")
	}
	hash, err := inner.GetSourceHash(context.Background(), path)
	if err != nil || hash != idx.currentSourceSignature(want).String() {
		t.Fatal("incremental redaction stored the wrong content signature")
	}
	writeFile(t, path, raw)
	chunkCalls.Store(0)
	embedCalls = 0
	if err := idx.IndexFileIncremental(context.Background(), path); err != nil || chunkCalls.Load() != 0 || embedCalls != 0 || store.clears.Load() != 1 {
		t.Fatal("unchanged sanitized source did not retain the hash no-op")
	}
	// A later stricter policy must re-scan even though the sanitized hash exists.
	strict := buildIndexer(stubEmbedder("p/m"), inner)
	if err := strict.IndexFileIncremental(context.Background(), path); !IsSafeIndexSkip(err) {
		t.Fatal("policy change did not clear previously redacted source")
	}
	requirePolicySourceAbsent(t, inner, path)
}

func TestIndexFilePolicyFailureRevocation(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		for _, phase := range []string{"skip clear", "redact clear", "chunk", "embed", "embedding count", "replace"} {
			t.Run(fmt.Sprintf("%t/%s", incremental, phase), func(t *testing.T) {
				idx, inner, calls := policyTestIndexer(t)
				if phase != "skip clear" {
					WithSensitiveRedaction(SensitiveOpenAIToken)(idx)
				}
				path := filepath.Join(t.TempDir(), "source.md")
				content := policyTestSecret()
				writeFile(t, path, content)
				seedPolicySource(t, idx, inner, path, content)
				cause := errors.New("injected failure " + policyTestSecret())
				store := &policyTestStore{SQLiteStore: inner}
				idx.store = store
				switch phase {
				case "skip clear", "redact clear":
					store.clearErr = cause
				case "chunk":
					idx.chunker = chunkerFunc(func(string, string) ([]Chunk, error) { return nil, cause })
				case "embed":
					idx.embedder = EmbedderFunc(func(context.Context, string, []string) (EmbedResult, error) { return EmbedResult{}, cause })
				case "embedding count":
					idx.embedder = EmbedderFunc(func(context.Context, string, []string) (EmbedResult, error) { return EmbedResult{}, nil })
					cause = ErrEmbeddingCountMismatch
				case "replace":
					store.publishErr = cause
				}
				var err error
				if incremental {
					err = idx.IndexFileIncremental(context.Background(), path)
				} else {
					var status IndexStatus
					status, err = idx.IndexFileWithStatus(context.Background(), path)
					if status.IndexedFiles != 0 || len(status.Errors) != 1 || len(status.PolicyOutcomes) != 1 {
						t.Fatal("failed policy file lost failure status")
					}
				}
				var policy *IndexPolicyError
				if !errors.As(err, &policy) || !errors.Is(err, cause) || IsSafeIndexSkip(err) {
					t.Fatal("policy failure lost classification or wrapped cause")
				}
				if strings.Contains(err.Error(), policyTestSecret()) {
					t.Fatal("policy failure disclosed injected cause")
				}
				wantUnsafe := strings.HasSuffix(phase, "clear")
				if policy.Outcome.Unsafe != wantUnsafe || (policy.Outcome.Action == IndexPolicySkip) != (phase == "skip clear") {
					t.Fatal("policy failure has incorrect action/unsafe metadata")
				}
				if wantUnsafe {
					chunks, loadErr := inner.GetBySource(context.Background(), path)
					if loadErr != nil || len(chunks) != 1 || !summaryStored(t, inner, path) || calls.Load() != 0 {
						t.Fatal("failed pre-clear changed old rows or called embedder")
					}
				} else {
					requirePolicySourceAbsent(t, inner, path)
				}
			})
		}
	}
}

func TestIndexFilePolicySupportedKinds(t *testing.T) {
	body := "aB3dE7fG9hJ2" + "kL4mN6pQ8rS0"
	for _, tc := range []struct {
		kind          SensitiveKind
		content, want string
	}{
		{SensitiveOpenAIToken, "sk-" + body, "[REDACTED_SECRET]"},
		{SensitiveGitHubToken, "ghp_" + body, "[REDACTED_SECRET]"},
		{SensitiveGitLabToken, "glpat-" + body, "[REDACTED_SECRET]"},
		{SensitiveSlackToken, "xoxb-" + body, "[REDACTED_SECRET]"},
		{SensitiveNPMToken, "npm_" + body, "[REDACTED_SECRET]"},
		{SensitiveBearerToken, "Bearer " + body, "Bearer [REDACTED_SECRET]"},
		{SensitiveSecretAssignment, "token=" + body, "token=[REDACTED_SECRET]"},
		{SensitivePrivateKey, "-----BEGIN " + "PRIVATE KEY-----\r\nbody\r\n-----END " + "PRIVATE KEY-----", "[REDACTED_SECRET]\r\n\r\n"},
		{SensitivePaymentCard, "45320198" + "7654321" + "5", "[REDACTED_PAYMENT_CARD]"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			idx, store, calls := policyTestIndexer(t)
			path := filepath.Join(t.TempDir(), "source.md")
			writeFile(t, path, tc.content)
			if err := idx.IndexFile(context.Background(), path); !IsSafeIndexSkip(err) || calls.Load() != 0 {
				t.Fatal("supported kind did not default to skip")
			}
			WithSensitiveRedaction(tc.kind)(idx)
			idx.chunker = chunkerFunc(func(source, content string) ([]Chunk, error) {
				return []Chunk{makeChunk(source, content, 1, 3, "")}, nil
			})
			status, err := idx.IndexFileWithStatus(context.Background(), path)
			if err != nil || len(status.PolicyOutcomes) != 1 || !reflect.DeepEqual(status.PolicyOutcomes[0].Kinds, []SensitiveKind{tc.kind}) {
				t.Fatal("selected supported kind did not redact successfully")
			}
			chunks, loadErr := store.GetBySource(context.Background(), path)
			if loadErr != nil || len(chunks) != 1 || chunks[0].Chunk.Content != tc.want {
				t.Fatal("supported kind placeholder or line breaks are incorrect")
			}
		})
	}
}

func TestIndexFilePolicyRedactionClosesExposedKinds(t *testing.T) {
	// The card replacement raises the surrounding scalar's entropy enough to
	// expose an assignment. The original canonical card kind still owns policy.
	card := strings.Repeat("0", 5) + "12" + "0" + "7" + strings.Repeat("0", 9)
	value := strings.Repeat("0", 6) + "\\" + card + "A" + strings.Repeat("0", 3) + "1"
	idx, store, _ := policyTestIndexer(t, WithSensitiveRedaction(SensitivePaymentCard))
	path := filepath.Join(t.TempDir(), "source.md")
	writeFile(t, path, "token=\""+value+"\"")
	status, err := idx.IndexFileWithStatus(context.Background(), path)
	if err != nil || len(status.PolicyOutcomes) != 1 || !reflect.DeepEqual(status.PolicyOutcomes[0].Kinds, []SensitiveKind{SensitivePaymentCard}) {
		t.Fatal("redaction changed original policy kinds or rejected an exposed kind")
	}
	chunks, loadErr := store.GetBySource(context.Background(), path)
	if loadErr != nil || len(chunks) != 1 || chunks[0].Chunk.Content != "token=\"[REDACTED_SECRET]\"" {
		t.Fatal("redaction left a newly exposed assignment available")
	}
}

func TestManagedIngestionBypassesFilesystemPolicy(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	content := policyTestSecret()
	document, err := managed.IngestText(context.Background(), "managed.md", content, DocumentOptions{})
	if err != nil || document.State != DocumentStateIndexed {
		t.Fatal("filesystem policy changed managed ingestion")
	}
	chunks, err := store.GetBySource(context.Background(), document.source)
	if err != nil || len(chunks) != 1 || chunks[0].Chunk.Content != content {
		t.Fatal("managed preparation traversed filesystem redaction")
	}
}
