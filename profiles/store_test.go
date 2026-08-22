package profiles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

func mustDoc(t *testing.T, body string) *config.Document {
	t.Helper()
	d, err := config.NewDocumentFromBytes([]byte(body), config.Origin{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func seedUserProfile(t *testing.T, root, slug, body string) {
	t.Helper()
	pdir := filepath.Join(root, "profiles")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, slug+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const storeFixtureBody = `{"providers":{"local":{"base_url":"http://localhost:1"}},
  "models":{"agent":{"name":"m1","provider":"local","type":"dense"}},
  "defaults":{"agent":"agent"}}`

// Load validates IDs before filesystem access (SaveAs/Export are Task 9 —
// their ID checks are tested there).
func TestLoadValidatesID(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Load(context.Background(), "user/../evil"); CodeOf(err) != CodeInvalidID {
		t.Fatalf("want invalid_id, got %v", err)
	}
}

// ABSENT root: List serves curated only; curated Load works; user Load is
// not_found; read paths create nothing.
func TestStoreAbsentRootReadBehavior(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	s := NewStore(root)
	infos, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) == 0 || !infos[0].Curated {
		t.Fatalf("absent root must still list curated: %+v", infos)
	}
	if _, err := s.Load(context.Background(), "curated/local"); err != nil {
		t.Fatalf("curated load with absent root: %v", err)
	}
	if _, err := s.Load(context.Background(), "user/none"); CodeOf(err) != CodeNotFound {
		t.Fatalf("user load with absent root: %v", err)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("read path created the root")
	}
}

// Symlinked root or symlinked profiles dir → store_unsafe.
func TestStoreRejectsSymlinkedRootAndProfilesDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(link).List(context.Background()); CodeOf(err) != CodeStoreUnsafe {
		t.Fatal("symlinked root must be refused")
	}
	root := t.TempDir()
	realProfiles := t.TempDir()
	if err := os.Symlink(realProfiles, filepath.Join(root, "profiles")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(root).List(context.Background()); CodeOf(err) != CodeStoreUnsafe {
		t.Fatal("symlinked profiles dir must be refused")
	}
}

// Entries: non-regular skipped in List and refused in Load; stems that do
// not parse as valid user IDs are skipped entirely.
func TestStoreEntryDiscipline(t *testing.T) {
	root := t.TempDir()
	seedUserProfile(t, root, "good", storeFixtureBody)
	pdir := filepath.Join(root, "profiles")
	if err := os.MkdirAll(filepath.Join(pdir, "adir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(pdir, "evil.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "Bad.json"), []byte(storeFixtureBody), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(root)
	infos, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, i := range infos {
		ids = append(ids, string(i.ID))
	}
	want := []string{"curated/local", "user/good"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v (non-regular + invalid stems skipped)", ids, want)
	}
	if _, err := s.Load(context.Background(), "user/evil"); CodeOf(err) != CodeStoreUnsafe {
		t.Fatal("symlinked profile must be refused in Load")
	}
}

// Load returns a Document with profile origin.
func TestLoadUserProfileOrigin(t *testing.T) {
	root := t.TempDir()
	seedUserProfile(t, root, "mine", storeFixtureBody)
	d, err := NewStore(root).Load(context.Background(), "user/mine")
	if err != nil {
		t.Fatal(err)
	}
	if d.Origin().Source != config.OriginProfile {
		t.Fatalf("origin = %+v", d.Origin())
	}
}

// P0 (round 3): Persisted==true ⇒ err == nil; warning travels INSIDE the
// outcome. Firn-style callers that discard results on non-nil error can
// never diverge from disk.
func TestSaveAsOutcomeErrorDiscipline(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	d := mustDoc(t, storeFixtureBody)
	out, err := s.SaveAs(context.Background(), "user/x", d, "")
	if err != nil || !out.Persisted || out.Warning != "" || out.Revision == "" {
		t.Fatalf("clean save: out=%+v err=%v", out, err)
	}
	if o := d.Origin(); o.Source != config.OriginProfile {
		t.Fatalf("origin = %+v", o)
	}
}

// Outcome mapping is pinned by the unexported helper — durability becomes
// a nil-error warning outcome; refusals map to typed codes with zero
// outcome. (No exported syncDir hook: the mapping logic is the unit.)
func TestSaveOutcomeFromErr(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		rev     string
		want    SaveOutcome
		wantErr ErrorCode // "" = nil error expected
	}{
		{"nil", nil, "r1", SaveOutcome{Persisted: true, Revision: "r1"}, ""},
		{"durability", fmt.Errorf("wrap: %w", config.ErrDurabilityUncertain), "r1",
			SaveOutcome{Persisted: true, Warning: CodeDurability, Revision: "r1"}, ""},
		{"exists", fmt.Errorf("wrap: %w", os.ErrExist), "r1", SaveOutcome{}, CodeConflict},
		{"revision", fmt.Errorf("wrap: %w", config.ErrRevisionConflict), "r1", SaveOutcome{}, CodeConflict},
		{"vanished target", fmt.Errorf("wrap: %w", os.ErrNotExist), "r1", SaveOutcome{}, CodeConflict},
		{"other", errors.New("disk on fire"), "r1", SaveOutcome{}, CodeIO},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := saveOutcomeFromErr(tc.err, tc.rev, ID("user/x"))
			if !reflect.DeepEqual(out, tc.want) {
				t.Fatalf("out = %+v, want %+v", out, tc.want)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantErr != "" && CodeOf(err) != tc.wantErr {
				t.Fatalf("err = %v, want %s", err, tc.wantErr)
			}
		})
	}
}

func TestSaveAsRefusalsAndOverwrite(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	d := mustDoc(t, storeFixtureBody)
	if _, err := s.SaveAs(context.Background(), "curated/local", d, ""); CodeOf(err) != CodeCuratedReadOnly {
		t.Fatalf("curated write: %v", err)
	}
	if _, err := s.SaveAs(context.Background(), "user/../evil", d, ""); CodeOf(err) != CodeInvalidID {
		t.Fatalf("invalid id: %v", err)
	}
	out, err := s.SaveAs(context.Background(), "user/x", d, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveAs(context.Background(), "user/x", d, ""); CodeOf(err) != CodeConflict {
		t.Fatalf("unversioned overwrite: %v", err)
	}
	if err := d.BindUseCase("agent", "agent"); err != nil {
		t.Fatal(err)
	}
	out2, err := s.SaveAs(context.Background(), "user/x", d, out.Revision)
	if err != nil || !out2.Persisted {
		t.Fatalf("versioned overwrite: out=%+v err=%v", out2, err)
	}
	reloaded, lerr := s.Load(context.Background(), "user/x")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if reloaded.Revision() != out2.Revision {
		t.Fatal("outcome revision does not match stored bytes")
	}
}

func TestSaveAsSecretLiteralsHold(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PROFILE_TEST_KEY", "sekret-value")
	body := `{"providers":{"local":{"base_url":"http://localhost:1","api_key":"${PROFILE_TEST_KEY}"}},
	  "models":{"agent":{"name":"m1","provider":"local","type":"dense"}},
	  "defaults":{"agent":"agent"}}`
	d := mustDoc(t, body)
	if _, err := NewStore(root).SaveAs(context.Background(), "user/sec", d, ""); err != nil {
		t.Fatal(err)
	}
	raw, rerr := os.ReadFile(filepath.Join(root, "profiles", "sec.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if bytes.Contains(raw, []byte("sekret-value")) || !bytes.Contains(raw, []byte("${PROFILE_TEST_KEY}")) {
		t.Fatal("secret handling violated") // no dump
	}
}

// Export is a host-CLI affordance (excluded from Wails): writes a loadable
// file, refuses existing destinations, validates IDs, and refuses symlinked
// user sources.
func TestExportDiscipline(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	dest := filepath.Join(t.TempDir(), "exported.json")
	if err := s.Export(context.Background(), "curated/local", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadDocument(dest); err != nil {
		t.Fatalf("exported profile does not load: %v", err)
	}
	if err := s.Export(context.Background(), "curated/local", dest); CodeOf(err) != CodeConflict {
		t.Fatalf("existing destination: %v", err)
	}
	if err := s.Export(context.Background(), "user/../evil", filepath.Join(t.TempDir(), "o.json")); CodeOf(err) != CodeInvalidID {
		t.Fatal("export must validate IDs")
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, []byte(storeFixtureBody), 0o600); err != nil {
		t.Fatal(err)
	}
	seedUserProfile(t, root, "real", storeFixtureBody)
	if err := os.Symlink(target, filepath.Join(root, "profiles", "sym.json")); err != nil {
		t.Fatal(err)
	}
	if err := s.Export(context.Background(), "user/sym", filepath.Join(t.TempDir(), "s.json")); CodeOf(err) != CodeStoreUnsafe {
		t.Fatalf("symlinked user source: %v", err)
	}
}

// Config-content failures classify as config_invalid (spec §8) with the
// config diagnostic reachable through the profiles wrapper; CodeIO stays
// filesystem-only.
func TestLoadConfigFailureClassifiedConfigInvalid(t *testing.T) {
	root := t.TempDir()
	seedUserProfile(t, root, "broken", `{"providers": {}}`)
	s := NewStore(root)
	_, err := s.Load(context.Background(), ID("user/broken"))
	if CodeOf(err) != CodeConfigInvalid {
		t.Fatalf("code = %q, want config_invalid", CodeOf(err))
	}
	// config diagnostic reaches THROUGH the profiles wrapper
	d, ok := config.DiagnosticOf(err)
	if !ok || d.Code != config.CodeProviderRequired {
		t.Fatalf("config diagnostic not reachable: %+v ok=%v", d, ok)
	}

	seedUserProfile(t, root, "bad-json", `{nope`)
	_, err = s.Load(context.Background(), ID("user/bad-json"))
	if CodeOf(err) != CodeConfigInvalid {
		t.Fatalf("parse failure code = %q", CodeOf(err))
	}
	if d, ok := config.DiagnosticOf(err); !ok || d.Code != config.CodeParseError {
		t.Fatalf("parse diagnostic = %+v ok=%v", d, ok)
	}

	// Read-only boundary pin: a collision profile loads read-only with NO
	// store error — Firn maps ReadOnly separately; a future edit fabricating
	// config_invalid for read-only docs must go red here.
	seedUserProfile(t, root, "dup", `{"providers":{"local":{"base_url":"http://localhost:1"}},
	  "models":{"agent":{"name":"m1","provider":"local","type":"dense"},
	            "agent":{"name":"m2","provider":"local","type":"dense"}},
	  "defaults":{"agent":"agent"}}`)
	doc, err := s.Load(context.Background(), ID("user/dup"))
	if err != nil {
		t.Fatalf("collision profile must load without store error: %v", err)
	}
	if ro, ok := doc.ReadOnly(); !ok || ro.Code != config.CodeDuplicateKeys {
		t.Fatalf("ReadOnly = %+v ok=%v, want duplicate_keys", ro, ok)
	}
}

// A collision-marked document refuses SaveAs/Export as CONTENT failures:
// config_invalid with the duplicate_keys diagnostic reachable through the
// profiles wrapper. CodeIO stays filesystem-only; nothing is persisted.
func TestReadOnlyRefusalClassifiedConfigInvalid(t *testing.T) {
	root := t.TempDir()
	seedUserProfile(t, root, "dup", `{"providers":{"local":{"base_url":"http://localhost:1"}},
	  "models":{"agent":{"name":"m1","provider":"local","type":"dense"},
	            "agent":{"name":"m2","provider":"local","type":"dense"}},
	  "defaults":{"agent":"agent"}}`)
	s := NewStore(root)
	doc, err := s.Load(context.Background(), ID("user/dup"))
	if err != nil {
		t.Fatal(err)
	}

	out, err := s.SaveAs(context.Background(), "user/copy", doc, "")
	if CodeOf(err) != CodeConfigInvalid {
		t.Fatalf("SaveAs code = %q (%v), want config_invalid", CodeOf(err), err)
	}
	if d, ok := config.DiagnosticOf(err); !ok || d.Code != config.CodeDuplicateKeys {
		t.Fatalf("SaveAs diagnostic = %+v ok=%v, want duplicate_keys", d, ok)
	}
	if out.Persisted || out.Warning != "" {
		t.Fatalf("refusal must not report persistence: %+v", out)
	}
	if _, serr := os.Lstat(filepath.Join(root, "profiles", "copy.json")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("refused SaveAs left a file: %v", serr)
	}

	dest := filepath.Join(t.TempDir(), "dup-export.json")
	err = s.Export(context.Background(), "user/dup", dest)
	if CodeOf(err) != CodeConfigInvalid {
		t.Fatalf("Export code = %q (%v), want config_invalid", CodeOf(err), err)
	}
	if d, ok := config.DiagnosticOf(err); !ok || d.Code != config.CodeDuplicateKeys {
		t.Fatalf("Export diagnostic = %+v ok=%v, want duplicate_keys", d, ok)
	}
	if _, serr := os.Lstat(dest); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("refused Export left a file: %v", serr)
	}
}

// NewStoreWithOptions threads DocumentOptions.LookupEnv into every profile
// Document the store parses (spec §7); NewStore stays ambient.
func TestStoreWithOptionsInjectsEnv(t *testing.T) {
	t.Setenv("PROF_KEY", "")
	root := t.TempDir()
	seedUserProfile(t, root, "cloudish", fmt.Sprintf(`{
	  "providers":{"p":{"base_url":"http://h","api_key":%q}},
	  "models":{"agent":{"name":"m","type":"dense","provider":"p"}},
	  "defaults":{"agent":"agent"}}`, "${PROF_KEY}"))
	snap := map[string]string{"PROF_KEY": "v"}
	s := NewStoreWithOptions(root, config.DocumentOptions{
		LookupEnv: func(k string) (string, bool) { v, ok := snap[k]; return v, ok },
	})
	if _, err := s.Load(context.Background(), ID("user/cloudish")); err != nil {
		t.Fatalf("injected env must satisfy the ref: %v", err)
	}
	// ambient store fails the same profile
	s2 := NewStore(root)
	_, err := s2.Load(context.Background(), ID("user/cloudish"))
	if CodeOf(err) != CodeConfigInvalid {
		t.Fatalf("ambient store: code = %q", CodeOf(err))
	}
}

func TestStoreContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewStore(t.TempDir())
	if _, err := s.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List err = %v", err)
	}
	if _, err := s.Load(ctx, "curated/local"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load err = %v", err)
	}
}
