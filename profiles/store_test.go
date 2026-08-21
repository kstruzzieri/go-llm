package profiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

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
