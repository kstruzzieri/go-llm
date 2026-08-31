package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// errSourceFailed is the sentinel for an already-rendered non-zero source
// outcome, mirroring errIndexFailed: main() exits 1 without double-printing.
var errSourceFailed = errors.New("golem source failed")

const sourceUsage = `usage: golem source <add|list|rm|reindex> [options] [arg]

  add [options] <path>    ingest a local UTF-8 file as a managed source
  add -text -name <name>  ingest stdin as a named managed text source
  list [options]          list managed sources (kind, state, freshness)
  rm [options] <id>       delete a managed source and its chunks
  reindex [options] <id>  re-read and re-embed a managed source

Flags must come before the positional path or id.
`

// rejectTrailingFlags catches stdlib flag's stop-at-first-positional
// behavior: a "-flag" after the positional argument would otherwise be
// misread as a second positional and produce a confusing count error.
func rejectTrailingFlags(fs *flag.FlagSet, command string, errOut io.Writer) bool {
	for _, arg := range fs.Args()[1:] {
		if len(arg) > 1 && arg[0] == '-' {
			_, _ = fmt.Fprintf(errOut, "golem source %s: flags must come before the positional argument (saw %q after it)\n", command, arg)
			return true
		}
	}
	return false
}

func validSourceDocumentID(command, id string, errOut io.Writer) bool {
	if validGenerationID(id) {
		return true
	}
	_, _ = fmt.Fprintf(errOut, "golem source %s: id must be a full 32-character lowercase hexadecimal document ID\n", command)
	return false
}

// sourceDeps carries test seams; zero value selects production behavior.
type sourceDeps struct {
	getenv              func(string) string // nil => os.Getenv
	embedder            rag.Embedder        // nil => build from the provider bundle
	embChain            []string            // used only when embedder is non-nil
	afterInputPreflight func()              // test-only file-change seam
}

func (d sourceDeps) env() func(string) string {
	if d.getenv != nil {
		return d.getenv
	}
	return os.Getenv
}

func runSource(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer) error {
	return runSourceWith(ctx, args, stdin, out, errOut, sourceDeps{})
}

func runSourceWith(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer, deps sourceDeps) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(errOut, sourceUsage)
		return errSourceFailed
	}
	switch args[0] {
	case "add":
		return runSourceAdd(ctx, args[1:], stdin, out, errOut, deps)
	case "list":
		return runSourceList(ctx, args[1:], out, errOut, deps)
	case "rm":
		return runSourceRm(ctx, args[1:], out, errOut, deps)
	case "reindex":
		return runSourceReindex(ctx, args[1:], out, errOut, deps)
	default:
		_, _ = fmt.Fprintf(errOut, "golem source: unknown source command %q\n%s", args[0], sourceUsage)
		return errSourceFailed
	}
}

// sourceWorkspace resolves the root and the per-workspace index identity.
func sourceWorkspace(rootFlag string, getenv func(string) string) (root, dbPath, workspaceID string, err error) {
	root, err = filepath.Abs(rootFlag)
	if err != nil {
		return "", "", "", fmt.Errorf("golem: resolve root: %w", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return "", "", "", fmt.Errorf("golem: resolve root: %w", err)
	}
	dbPath, workspaceID, err = indexDBPathForWorkspace(getenv, root)
	if err != nil {
		return "", "", "", err
	}
	return root, dbPath, workspaceID, nil
}

// tagsFlag collects repeatable -tag values.
type tagsFlag []string

func (t *tagsFlag) String() string { return fmt.Sprint([]string(*t)) }
func (t *tagsFlag) Set(v string) error {
	*t = append(*t, v)
	return nil
}

func runSourceAdd(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer, deps sourceDeps) error {
	var (
		configPath, name, title, collection, ollamaURL string
		text                                           bool
		tags                                           tagsFlag
		rootFlag                                       string
	)
	fs := flag.NewFlagSet("golem source add", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&configPath, "config", "", "path to models.json (default: auto-discover)")
	fs.StringVar(&rootFlag, "root", ".", "workspace root")
	fs.StringVar(&ollamaURL, "ollama-url", "", "override Ollama base URL")
	fs.BoolVar(&text, "text", false, "ingest stdin as a named text document")
	fs.StringVar(&name, "name", "", "document name (required with -text)")
	fs.StringVar(&title, "title", "", "optional display title")
	fs.StringVar(&collection, "collection", "", "optional collection")
	fs.Var(&tags, "tag", "optional tag (repeatable)")
	var allowDest stringSliceFlag
	fs.Var(&allowDest, "allow-destination", "admit a remote model destination: \"<provider>/<canonical base URL>\" (repeatable; this command never prompts)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errSourceFailed
	}
	if fs.NArg() > 1 && rejectTrailingFlags(fs, "add", errOut) {
		return errSourceFailed
	}
	switch {
	case text && name == "":
		_, _ = fmt.Fprintln(errOut, "golem source add: -name is required with -text")
		return errSourceFailed
	case text && fs.NArg() > 0:
		_, _ = fmt.Fprintln(errOut, "golem source add: cannot combine -text with a path")
		return errSourceFailed
	case !text && fs.NArg() == 0:
		_, _ = fmt.Fprintln(errOut, "golem source add: path is required (or use -text -name)")
		return errSourceFailed
	case !text && fs.NArg() > 1:
		_, _ = fmt.Fprintln(errOut, "golem source add: exactly one path is accepted")
		return errSourceFailed
	}
	return sourceAddExecute(ctx, sourceAddSpec{
		configPath: configPath, rootFlag: rootFlag, ollamaURL: ollamaURL,
		allowDest: allowDest,
		text:      text, name: name, path: firstArg(fs),
		opts: rag.DocumentOptions{Title: title, Collection: collection, Tags: []string(tags)},
	}, stdin, out, errOut, deps)
}

func firstArg(fs *flag.FlagSet) string {
	if fs.NArg() > 0 {
		return fs.Arg(0)
	}
	return ""
}

type sourceAddSpec struct {
	configPath, rootFlag, ollamaURL string
	allowDest                       []string
	text                            bool
	name, path                      string
	opts                            rag.DocumentOptions
}

// sourceEmbedderFor returns the embedder + chain for a mutating command: the
// injected test seam when present, else a provider bundle built from config.
// The returned closer releases bundle resources (no-op for the seam).
func sourceEmbedderFor(ctx context.Context, configPath, ollamaURL string, allowDest []string, deps sourceDeps, errOut io.Writer) (rag.Embedder, []string, func(), error) {
	if deps.embedder != nil {
		return deps.embedder, deps.embChain, func() {}, nil
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	// #477: the embedding route is the command's whole reachability; admit
	// it (allowlist only, never a prompt) before bootstrap.
	embChain, err := embeddingChain(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	gate, netPlan, _, err := admitForSubcommand(ctx, cfg,
		[]providerbootstrap.PlannedRoute{{UseCase: "embedding", Chain: embChain}},
		providerbootstrap.PlanOptions{}, allowDest, ollamaURL, "", "", errOut)
	if err != nil {
		return nil, nil, nil, err
	}
	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{
		Config:            cfg,
		OllamaURLOverride: ollamaURL,
		DestinationGate:   gate,
		ActiveProviders:   netPlan.ActiveProviders,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("golem: bootstrap providers: %w", err)
	}
	closer := func() {
		if closeErr := bundle.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(errOut, "golem: close provider bundle: %v\n", closeErr)
		}
	}
	embedder := newChainEmbedder(func(rc context.Context, rr provider.RoutingRequest) (embedExecutor, error) {
		return bundle.Router.Route(rc, rr)
	}, embChain)
	return embedder, embChain, closer, nil
}

func sourceAddExecute(ctx context.Context, spec sourceAddSpec, stdin io.Reader, out, errOut io.Writer, deps sourceDeps) error {
	_, dbPath, workspaceID, err := sourceWorkspace(spec.rootFlag, deps.env())
	if err != nil {
		return err
	}
	var content string
	if spec.text {
		data, err := io.ReadAll(io.LimitReader(stdin, rag.MaxManagedDocumentBytes+1))
		if err != nil {
			return fmt.Errorf("golem source add: read stdin: %w", err)
		}
		if len(data) == 0 {
			_, _ = fmt.Fprintln(errOut, "golem source add: stdin must not be empty")
			return errSourceFailed
		}
		if len(data) > rag.MaxManagedDocumentBytes {
			_, _ = fmt.Fprintf(errOut, "golem source add: stdin exceeds %d-byte managed document limit\n", rag.MaxManagedDocumentBytes)
			return errSourceFailed
		}
		content = string(data)
	} else {
		info, err := os.Stat(spec.path)
		if err != nil {
			_, _ = fmt.Fprintf(errOut, "golem source add: inspect %q: %v\n", spec.path, err)
			return errSourceFailed
		}
		if info.Size() == 0 {
			_, _ = fmt.Fprintln(errOut, "golem source add: file must not be empty")
			return errSourceFailed
		}
	}
	if deps.afterInputPreflight != nil {
		deps.afterInputPreflight()
	}
	embedder, embChain, closeBundle, err := sourceEmbedderFor(ctx, spec.configPath, spec.ollamaURL, spec.allowDest, deps, errOut)
	if err != nil {
		return err
	}
	defer closeBundle()

	lease, err := acquireIndexWriterLease(dbPath)
	if errors.Is(err, errIndexWriterLeaseHeld) {
		_, _ = fmt.Fprintln(errOut, "golem source add: workspace index writer lease already held; another golem process is indexing")
		return errSourceFailed
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source add: %v\n", err)
		return errSourceFailed
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(errOut, "golem: close index writer lease: %v\n", closeErr)
		}
	}()

	actualVectorSpace, err := probeAutoIndexEmbedder(ctx, embedder, embChain[0])
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source add: %v\n", err)
		return errSourceFailed
	}
	var doc rag.Document
	built, err := buildIndexGeneration(ctx, generationBuildOptions{
		dbPath:              dbPath,
		workspaceID:         workspaceID,
		requestedModel:      embChain[0],
		actualVectorSpace:   actualVectorSpace,
		embedder:            embedder,
		refuseInvalidActive: true,
		out:                 errOut, // builder progress/warnings are operational, not primary output
		mutate: func(mctx context.Context, indexer *rag.Indexer, store *rag.SQLiteStore) error {
			managed, err := rag.NewManagedSources(indexer, store)
			if err != nil {
				return err
			}
			if spec.text {
				doc, err = managed.IngestText(mctx, spec.name, content, spec.opts)
			} else {
				doc, err = managed.IngestFile(mctx, spec.path, spec.opts)
				if err == nil && doc.ChunkCount == 0 {
					return errors.New("file produced no indexable content")
				}
			}
			return err
		},
	})
	if built.gcWarn != "" {
		_, _ = fmt.Fprintf(errOut, "golem source add: warning: %s\n", built.gcWarn)
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source add: %v\n", err)
		return errSourceFailed
	}
	if err := publishSourceGeneration(ctx, dbPath, workspaceID, built); err != nil {
		return err
	}
	if err := printSourceDocument(out, doc); err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source add: write result: %v\n", err)
		return errSourceFailed
	}
	return nil
}

// publishSourceGeneration publishes a built generation with runIndex's
// unpublished-cleanup semantics.
func publishSourceGeneration(ctx context.Context, dbPath, workspaceID string, built generationBuildResult) error {
	pointer := activeGenerationPointer{SchemaVersion: activePointerSchemaVersion, WorkspaceID: workspaceID, Generation: built.generation.id}
	if err := publishActiveGeneration(ctx, dbPath, pointer, nil); err != nil {
		if shouldRemoveUnpublished(context.WithoutCancel(ctx), dbPath, built.generation.id) {
			if cleanupErr := removeGenerationPath(context.WithoutCancel(ctx), filepath.Dir(built.generation.dbPath)); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
		return err
	}
	return nil
}

func printSourceDocument(out io.Writer, doc rag.Document) error {
	origin := doc.Origin
	if origin == "" {
		origin = doc.Title
	}
	_, err := fmt.Fprintf(out, "%s %s %s (%d chunks) -> %s\n", doc.ID, doc.Kind, doc.State, doc.ChunkCount, sourceHumanText(origin))
	return err
}

func sourceHumanText(value string) string {
	return strconv.QuoteToGraphic(value)
}

func runSourceRm(ctx context.Context, args []string, out, errOut io.Writer, deps sourceDeps) error {
	var rootFlag string
	fs := flag.NewFlagSet("golem source rm", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&rootFlag, "root", ".", "workspace root")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errSourceFailed
	}
	if fs.NArg() == 0 {
		_, _ = fmt.Fprintln(errOut, "golem source rm: id is required")
		return errSourceFailed
	}
	if fs.NArg() > 1 {
		if rejectTrailingFlags(fs, "rm", errOut) {
			return errSourceFailed
		}
		_, _ = fmt.Fprintln(errOut, "golem source rm: exactly one id is accepted")
		return errSourceFailed
	}
	id := fs.Arg(0)
	if !validSourceDocumentID("rm", id, errOut) {
		return errSourceFailed
	}
	return sourceRmExecute(ctx, rootFlag, id, out, errOut, deps)
}

func sourceRmExecute(ctx context.Context, rootFlag, id string, out, errOut io.Writer, deps sourceDeps) error {
	_, dbPath, workspaceID, err := sourceWorkspace(rootFlag, deps.env())
	if err != nil {
		return err
	}
	lease, err := acquireIndexWriterLease(dbPath)
	if errors.Is(err, errIndexWriterLeaseHeld) {
		_, _ = fmt.Fprintln(errOut, "golem source rm: workspace index writer lease already held; another golem process is indexing")
		return errSourceFailed
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source rm: %v\n", err)
		return errSourceFailed
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(errOut, "golem: close index writer lease: %v\n", closeErr)
		}
	}()
	active, err := resolveActiveGeneration(ctx, dbPath, workspaceID)
	if errors.Is(err, errNoActiveGeneration) {
		_, _ = fmt.Fprintln(errOut, "golem source rm: no workspace index")
		return errSourceFailed
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source rm: %v\n", err)
		return errSourceFailed
	}
	built, err := buildIndexGeneration(ctx, generationBuildOptions{
		dbPath:              dbPath,
		workspaceID:         workspaceID,
		requestedModel:      active.metadata.RequestedEmbeddingModel,
		actualVectorSpace:   active.metadata.VectorSpaceID,
		refuseInvalidActive: true,
		out:                 errOut,
		mutate: func(mctx context.Context, _ *rag.Indexer, store *rag.SQLiteStore) error {
			managed, err := rag.NewManagedSources(nil, store)
			if err != nil {
				return err
			}
			return managed.DeleteDocument(mctx, id)
		},
	})
	if built.gcWarn != "" {
		_, _ = fmt.Fprintf(errOut, "golem source rm: warning: %s\n", built.gcWarn)
	}
	if err != nil {
		if errors.Is(err, rag.ErrDocumentNotFound) {
			_, _ = fmt.Fprintf(errOut, "golem source rm: managed document not found: %s\n", id)
			return errSourceFailed
		}
		if errors.Is(err, errEmptyStagingGeneration) {
			if err := retireActiveGeneration(ctx, dbPath, workspaceID); err != nil {
				_, _ = fmt.Fprintf(errOut, "golem source rm: retire empty workspace index: %v\n", err)
				return errSourceFailed
			}
			if _, err := fmt.Fprintf(out, "deleted %s\n", id); err != nil {
				_, _ = fmt.Fprintf(errOut, "golem source rm: write result: %v\n", err)
				return errSourceFailed
			}
			return nil
		}
		_, _ = fmt.Fprintf(errOut, "golem source rm: %v\n", err)
		return errSourceFailed
	}
	if err := publishSourceGeneration(ctx, dbPath, workspaceID, built); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "deleted %s\n", id); err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source rm: write result: %v\n", err)
		return errSourceFailed
	}
	return nil
}

func runSourceReindex(ctx context.Context, args []string, out, errOut io.Writer, deps sourceDeps) error {
	var configPath, rootFlag, ollamaURL string
	var allowDest stringSliceFlag
	fs := flag.NewFlagSet("golem source reindex", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&configPath, "config", "", "path to models.json (default: auto-discover)")
	fs.StringVar(&rootFlag, "root", ".", "workspace root")
	fs.StringVar(&ollamaURL, "ollama-url", "", "override Ollama base URL")
	fs.Var(&allowDest, "allow-destination", "admit a remote model destination: \"<provider>/<canonical base URL>\" (repeatable; this command never prompts)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errSourceFailed
	}
	if fs.NArg() == 0 {
		_, _ = fmt.Fprintln(errOut, "golem source reindex: id is required")
		return errSourceFailed
	}
	if fs.NArg() > 1 {
		if rejectTrailingFlags(fs, "reindex", errOut) {
			return errSourceFailed
		}
		_, _ = fmt.Fprintln(errOut, "golem source reindex: exactly one id is accepted")
		return errSourceFailed
	}
	id := fs.Arg(0)
	if !validSourceDocumentID("reindex", id, errOut) {
		return errSourceFailed
	}
	return sourceReindexExecute(ctx, configPath, rootFlag, ollamaURL, allowDest, id, out, errOut, deps)
}

func sourceReindexExecute(ctx context.Context, configPath, rootFlag, ollamaURL string, allowDest []string, id string, out, errOut io.Writer, deps sourceDeps) error {
	_, dbPath, workspaceID, err := sourceWorkspace(rootFlag, deps.env())
	if err != nil {
		return err
	}
	lease, err := acquireIndexWriterLease(dbPath)
	if errors.Is(err, errIndexWriterLeaseHeld) {
		_, _ = fmt.Fprintln(errOut, "golem source reindex: workspace index writer lease already held; another golem process is indexing")
		return errSourceFailed
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source reindex: %v\n", err)
		return errSourceFailed
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(errOut, "golem: close index writer lease: %v\n", closeErr)
		}
	}()
	if _, err := resolveActiveGeneration(ctx, dbPath, workspaceID); errors.Is(err, errNoActiveGeneration) {
		_, _ = fmt.Fprintln(errOut, "golem source reindex: no workspace index")
		return errSourceFailed
	} else if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source reindex: %v\n", err)
		return errSourceFailed
	}
	embedder, embChain, closeBundle, err := sourceEmbedderFor(ctx, configPath, ollamaURL, allowDest, deps, errOut)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source reindex: %v\n", err)
		return errSourceFailed
	}
	defer closeBundle()
	actualVectorSpace, err := probeAutoIndexEmbedder(ctx, embedder, embChain[0])
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source reindex: %v\n", err)
		return errSourceFailed
	}
	var doc rag.Document
	built, err := buildIndexGeneration(ctx, generationBuildOptions{
		dbPath:              dbPath,
		workspaceID:         workspaceID,
		requestedModel:      embChain[0],
		actualVectorSpace:   actualVectorSpace,
		embedder:            embedder,
		refuseInvalidActive: true,
		out:                 errOut,
		mutate: func(mctx context.Context, indexer *rag.Indexer, store *rag.SQLiteStore) error {
			managed, err := rag.NewManagedSources(indexer, store)
			if err != nil {
				return err
			}
			doc, err = managed.ReindexDocument(mctx, id)
			return err
		},
	})
	if built.gcWarn != "" {
		_, _ = fmt.Fprintf(errOut, "golem source reindex: warning: %s\n", built.gcWarn)
	}
	if err != nil {
		if errors.Is(err, rag.ErrDocumentNotFound) {
			_, _ = fmt.Fprintf(errOut, "golem source reindex: managed document not found: %s\n", id)
			return errSourceFailed
		}
		_, _ = fmt.Fprintf(errOut, "golem source reindex: %v\n", err)
		return errSourceFailed
	}
	if err := publishSourceGeneration(ctx, dbPath, workspaceID, built); err != nil {
		return err
	}
	if err := printSourceDocument(out, doc); err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source reindex: write result: %v\n", err)
		return errSourceFailed
	}
	return nil
}

func runSourceList(ctx context.Context, args []string, out, errOut io.Writer, deps sourceDeps) error {
	var rootFlag string
	var asJSON bool
	fs := flag.NewFlagSet("golem source list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&rootFlag, "root", ".", "workspace root")
	fs.BoolVar(&asJSON, "json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errSourceFailed
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintln(errOut, "golem source list: no positional arguments")
		return errSourceFailed
	}
	return sourceListExecute(ctx, rootFlag, asJSON, out, errOut, deps)
}

// sourceDocumentLister is the pagination seam over ManagedSources.
type sourceDocumentLister interface {
	ListDocuments(context.Context, rag.DocumentFilter) ([]rag.Document, error)
}

// collectSourceDocuments drains every unfiltered page: it resumes bounded-scan
// cursors and advances past full pages until a short page arrives. The returned
// slice is never nil so JSON output is [] not null.
func collectSourceDocuments(ctx context.Context, lister sourceDocumentLister) ([]rag.Document, error) {
	docs := make([]rag.Document, 0)
	filter := rag.DocumentFilter{}
	for {
		page, err := lister.ListDocuments(ctx, filter)
		docs = append(docs, page...)
		var scanErr *rag.ManagedListScanLimitError
		switch {
		case errors.As(err, &scanErr):
			filter.AfterID = scanErr.AfterID
		case err != nil:
			return nil, err
		case len(page) == rag.MaxManagedListLimit:
			filter.AfterID = page[len(page)-1].ID
		default:
			return docs, nil
		}
	}
}

// openResolvedSourceListStore retries one resolve/open gap. modernc SQLite
// reports a deleted read-only DB as SQLITE_CANTOPEN rather than wrapping
// os.ErrNotExist, so a failed open is followed by a path existence check.
func openResolvedSourceListStore(
	ctx context.Context,
	dbPath, workspaceID string,
	generation indexGeneration,
	resolve func(context.Context, string, string) (indexGeneration, error),
	open func(string) (*rag.SQLiteStore, error),
) (*rag.SQLiteStore, error) {
	store, err := open(generation.dbPath)
	if err == nil {
		return store, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(generation.dbPath); !errors.Is(statErr, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	generation, err = resolve(ctx, dbPath, workspaceID)
	if err != nil {
		return nil, err
	}
	return open(generation.dbPath)
}

func sourceListNoIndex(asJSON bool, out, errOut io.Writer) error {
	_, _ = fmt.Fprintln(errOut, "golem source list: no workspace index; nothing to list")
	if !asJSON {
		return nil
	}
	if err := renderSourceDocuments(out, []rag.Document{}, true); err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source list: %v\n", err)
		return errSourceFailed
	}
	return nil
}

func sourceListExecute(ctx context.Context, rootFlag string, asJSON bool, out, errOut io.Writer, deps sourceDeps) error {
	_, dbPath, workspaceID, err := sourceWorkspace(rootFlag, deps.env())
	if err != nil {
		return err
	}
	gen, err := resolveActiveGeneration(ctx, dbPath, workspaceID)
	if errors.Is(err, errNoActiveGeneration) {
		return sourceListNoIndex(asJSON, out, errOut)
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source list: %v\n", err)
		return errSourceFailed
	}
	store, err := openResolvedSourceListStore(
		ctx, dbPath, workspaceID, gen, resolveActiveGeneration, rag.OpenSQLiteStoreReadOnly)
	if errors.Is(err, errNoActiveGeneration) {
		return sourceListNoIndex(asJSON, out, errOut)
	}
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source list: open generation: %v\n", err)
		return errSourceFailed
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(errOut, "golem: close generation store: %v\n", closeErr)
		}
	}()
	managed, err := rag.NewManagedSources(nil, store)
	if err != nil {
		return err
	}
	docs, err := collectSourceDocuments(ctx, managed)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source list: %v\n", err)
		return errSourceFailed
	}
	if err := renderSourceDocuments(out, docs, asJSON); err != nil {
		_, _ = fmt.Fprintf(errOut, "golem source list: %v\n", err)
		return errSourceFailed
	}
	return nil
}

func renderSourceDocuments(out io.Writer, docs []rag.Document, asJSON bool) error {
	if asJSON {
		data, err := json.MarshalIndent(docs, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		if _, err := fmt.Fprintln(out, string(data)); err != nil {
			return fmt.Errorf("write JSON: %w", err)
		}
		return nil
	}
	return renderSourceTable(out, docs)
}

func renderSourceTable(out io.Writer, docs []rag.Document) error {
	if len(docs) == 0 {
		_, err := fmt.Fprintln(out, "no managed sources")
		return err
	}
	w := tabwriter.NewWriter(out, 2, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tKIND\tSTATE\tFRESHNESS\tCHUNKS\tUPDATED\tTITLE"); err != nil {
		return err
	}
	for _, doc := range docs {
		title := doc.Title
		if doc.Kind == rag.DocumentKindFile && doc.Origin != "" {
			title = doc.Origin
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			doc.ID, doc.Kind, doc.State, doc.Freshness, doc.ChunkCount,
			time.Unix(doc.UpdatedAt, 0).UTC().Format(time.RFC3339), sourceHumanText(title)); err != nil {
			return err
		}
		if doc.LastError != "" {
			if _, err := fmt.Fprintf(w, "\tlast error: %s\n", sourceHumanText(doc.LastError)); err != nil {
				return err
			}
		}
	}
	return w.Flush()
}
