package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// saveExposure saves a fresh conversation carrying e and returns its ID.
func saveExposure(t *testing.T, store *SQLiteStore, e Exposure) string {
	t.Helper()
	id := NewID()
	if _, err := store.Save(context.Background(), Conversation{ID: id, Messages: []Message{{Role: "user", Content: "hi"}}, Exposure: e}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return id
}

func loadExposure(t *testing.T, store *SQLiteStore, id string) Exposure {
	t.Helper()
	conv, err := store.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return conv.Exposure
}

// setRawExposure writes exposure columns by raw SQL, bound to the row's
// current revision unless rev says otherwise.
func setRawExposure(t *testing.T, store *SQLiteStore, id string, exposure, rev any) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE conversations SET exposure = ?, exposure_revision = ? WHERE id = ?`, exposure, rev, id); err != nil {
		t.Fatalf("raw exposure update: %v", err)
	}
}

func currentRevision(t *testing.T, store *SQLiteStore, id string) int64 {
	t.Helper()
	var rev int64
	if err := store.db.QueryRow(`SELECT revision FROM conversations WHERE id = ?`, id).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestExposureRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		e    Exposure
	}{
		{"foreign with label", Exposure{Recorded: true, Foreign: true, First: "tool:x"}},
		{"both flags", Exposure{Recorded: true, Foreign: true, Unknown: true, First: "tool:x"}},
		{"foreign empty first", Exposure{Recorded: true, Foreign: true}},
		{"recorded clean", Exposure{Recorded: true}},
		{"label with HTML and non-ASCII", Exposure{Recorded: true, Unknown: true, First: "tool:<a>&</a> é日本"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			id := saveExposure(t, store, tt.e)
			conv, err := store.Load(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if conv.Exposure != tt.e {
				t.Fatalf("Exposure = %+v, want %+v", conv.Exposure, tt.e)
			}
			// A second Save rebinds the record to the new revision.
			next := tt.e
			next.Foreign, next.First = true, "second"
			conv.Exposure = next
			if _, err := store.Save(ctx, *conv); err != nil {
				t.Fatalf("second Save: %v", err)
			}
			if got := loadExposure(t, store, id); got != next {
				t.Fatalf("after second Save Exposure = %+v, want %+v", got, next)
			}
		})
	}
	t.Run("clean exposure is not Exposed", func(t *testing.T) {
		store := newTestStore(t)
		got := loadExposure(t, store, saveExposure(t, store, Exposure{Recorded: true}))
		if !got.Recorded || got.Exposed() {
			t.Fatalf("Exposure = %+v, want recorded and not exposed", got)
		}
	})
}

func TestExposureLegacyRowReadsNotRecorded(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrationsWith(t.Context(), db, migrations[:5]); err != nil {
		t.Fatal(err)
	}
	execMigrationSQL(t, db, `INSERT INTO conversations (id,title,messages,summary_content,summary_message_count,revision,created_at,updated_at)
		VALUES ('legacy','t','[]','',0,7,1,2)`)
	assertMigrationSQL(t, db, `SELECT COUNT(*) FROM pragma_table_info('conversations') WHERE name LIKE 'exposure%'`, "0")
	store, err := NewStore(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	got := loadExposure(t, store, "legacy")
	if got.Recorded || !got.Exposed() {
		t.Fatalf("legacy Exposure = %+v, want not recorded and exposed", got)
	}
}

func TestExposureStoredValueReadsNotRecorded(t *testing.T) {
	const ok = `{"v":1,"foreign":true,"unknown":false,"first":"tool:x"}`
	tests := []struct {
		name string
		raw  any
	}{
		{"invalid JSON", `{"v":1,`},
		{"unsupported version", `{"v":2,"foreign":false,"unknown":false,"first":""}`},
		{"version zero", `{"v":0,"foreign":false,"unknown":false,"first":""}`},
		{"oversized first", fmt.Sprintf(`{"v":1,"foreign":true,"unknown":false,"first":%q}`, strings.Repeat("a", MaxExposureFirstBytes+1))},
		{"only v", `{"v":1}`},
		{"missing v", `{"foreign":true,"unknown":false,"first":""}`},
		{"missing foreign", `{"v":1,"unknown":false,"first":""}`},
		{"missing unknown", `{"v":1,"foreign":false,"first":""}`},
		{"missing first", `{"v":1,"foreign":false,"unknown":false}`},
		{"unknown key", `{"v":1,"foreign":true,"unknown":false,"first":"","extra":1}`},
		{"trailing object", ok + `{}`},
		{"trailing garbage", ok + ` x`},
		{"trailing brace", ok + `}`},
		{"duplicate key, last wins", `{"v":1,"foreign":true,"unknown":false,"first":"","foreign":false}`},
		{"upper-case keys", `{"V":1,"FOREIGN":false,"Unknown":false,"First":""}`},
		{"extra whitespace", `{"v": 1, "foreign": false, "unknown": false, "first": ""}`},
		{"key order", `{"first":"","v":1,"foreign":false,"unknown":false}`},
		{"JSON null", `null`},
		{"JSON array", `[]`},
		{"wrong type", `{"v":1,"foreign":"yes","unknown":false,"first":""}`},
		{"integer column", int64(5)},
		{"blob column", []byte(ok)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			id := saveExposure(t, store, Exposure{})
			setRawExposure(t, store, id, tt.raw, currentRevision(t, store, id))
			if got := loadExposure(t, store, id); got.Recorded {
				t.Fatalf("Exposure = %+v, want not recorded", got)
			}
		})
	}
	t.Run("control: valid value reads recorded", func(t *testing.T) {
		store := newTestStore(t)
		id := saveExposure(t, store, Exposure{})
		setRawExposure(t, store, id, ok, currentRevision(t, store, id))
		want := Exposure{Recorded: true, Foreign: true, First: "tool:x"}
		if got := loadExposure(t, store, id); got != want {
			t.Fatalf("Exposure = %+v, want %+v", got, want)
		}
	})
}

func TestExposureRevisionColumnShapesReadNotRecorded(t *testing.T) {
	const ok = `{"v":1,"foreign":false,"unknown":false,"first":""}`
	tests := []struct {
		name string
		rev  any
	}{
		{"text", "abc"},
		{"real", 1.5},
		{"NULL", nil},
		{"stale integer", int64(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			id := saveExposure(t, store, Exposure{})
			setRawExposure(t, store, id, ok, tt.rev)
			if got := loadExposure(t, store, id); got.Recorded {
				t.Fatalf("Exposure = %+v, want not recorded", got)
			}
		})
	}
}

func TestExposureNullExposureColumnReadsNotRecorded(t *testing.T) {
	store := newTestStore(t)
	id := saveExposure(t, store, Exposure{Recorded: true})
	setRawExposure(t, store, id, nil, currentRevision(t, store, id))
	if got := loadExposure(t, store, id); got.Recorded {
		t.Fatalf("Exposure = %+v, want not recorded", got)
	}
}

func TestExposureOlderBinaryUpdateReadsNotRecorded(t *testing.T) {
	store := newTestStore(t)
	id := saveExposure(t, store, Exposure{Recorded: true}) // clean
	if _, err := store.db.Exec(`UPDATE conversations SET messages = '[]', revision = revision + 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	got := loadExposure(t, store, id)
	if got.Recorded || !got.Exposed() {
		t.Fatalf("Exposure = %+v, want not recorded after an older binary's write", got)
	}
}

func TestExposureSaveNotRecordedStoresNull(t *testing.T) {
	store := newTestStore(t)
	id := saveExposure(t, store, Exposure{Foreign: true, First: "ignored"})
	var raw sql.NullString
	var rev sql.NullInt64
	if err := store.db.QueryRow(`SELECT exposure, exposure_revision FROM conversations WHERE id = ?`, id).Scan(&raw, &rev); err != nil {
		t.Fatal(err)
	}
	if raw.Valid {
		t.Fatalf("exposure = %q, want NULL", raw.String)
	}
	if want := currentRevision(t, store, id); !rev.Valid || rev.Int64 != want {
		t.Fatalf("exposure_revision = %+v, want committed revision %d even when exposure is NULL", rev, want)
	}
}

func TestExposureStoredWireForm(t *testing.T) {
	store := newTestStore(t)
	id := saveExposure(t, store, Exposure{Recorded: true, Foreign: true, First: "tool:x"})
	var raw string
	if err := store.db.QueryRow(`SELECT exposure FROM conversations WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if want := `{"v":1,"foreign":true,"unknown":false,"first":"tool:x"}`; raw != want {
		t.Fatalf("stored = %s, want %s", raw, want)
	}
}

func TestExposureConflictKeepsStoredValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	want := Exposure{Recorded: true, Foreign: true, First: "tool:x"}
	id := saveExposure(t, store, want)
	conv, err := store.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	stale := *conv
	stale.Revision--
	stale.Exposure = Exposure{Recorded: true}
	_, err = store.Save(ctx, stale)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("Save(stale) error = %v, want *ConflictError", err)
	}
	if got := loadExposure(t, store, id); got != want {
		t.Fatalf("Exposure after conflict = %+v, want %+v", got, want)
	}
}

func TestExposureBoundsFirstOnSave(t *testing.T) {
	tests := []struct {
		name  string
		first string
		want  string
	}{
		{"oversized ASCII", "tool:" + strings.Repeat("a", 300), "tool:" + strings.Repeat("a", 123)},
		// 200 bytes of two-byte runes: the 128-byte bound lands on a rune start, 64 runes.
		{"oversized multi-byte", strings.Repeat("é", 100), strings.Repeat("é", 64)},
		// 127 bytes, then a 3-byte rune straddling the 128-byte bound: cut before it.
		{"multi-byte straddling the bound", "tool:" + strings.Repeat("a", 122) + "日本", "tool:" + strings.Repeat("a", 122)},
		{"invalid UTF-8", "tool:" + strings.Repeat("\xff", 128), "tool:"},
		// 100 x "a\xff" strips to 100 "a"; all fit in the bound.
		{"invalid UTF-8 inside long label", strings.Repeat("a\xff", 100), strings.Repeat("a", 100)},
		// 200 x "a\xff" strips to 200 "a", cut to 128.
		{"invalid UTF-8 over the bound", strings.Repeat("a\xff", 200), strings.Repeat("a", 128)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			got := loadExposure(t, store, saveExposure(t, store, Exposure{Recorded: true, Foreign: true, First: tt.first}))
			if !got.Recorded || !got.Foreign {
				t.Fatalf("Exposure = %+v, want recorded and foreign", got)
			}
			if got.First != tt.want {
				t.Fatalf("First = %q (%d bytes), want %q (%d bytes)", got.First, len(got.First), tt.want, len(tt.want))
			}
		})
	}
}

func TestExposureSurvivesCompression(t *testing.T) {
	var msgs []Message
	for range 6 {
		msgs = append(msgs, makeMsg("user", strings.Repeat("u", 40)), makeMsg("assistant", strings.Repeat("a", 40)))
	}
	want := Exposure{Recorded: true, Foreign: true, First: "tool:x"}
	out, err := CompressMessagesWithCost(context.Background(), Conversation{ID: "c", Messages: msgs, Exposure: want}, 100, 1,
		func(m Message) int { return len(m.Content) }, okSummarizer)
	if err != nil {
		t.Fatal(err)
	}
	if out.DurableSummary == nil {
		t.Fatal("conversation was not compressed; test would prove nothing")
	}
	if out.Exposure != want {
		t.Fatalf("Exposure after compression = %+v, want %+v", out.Exposure, want)
	}
}

func TestExposureSurvivesJSON(t *testing.T) {
	want := Conversation{ID: "c", Exposure: Exposure{Recorded: true, Foreign: true, First: "tool:x"}}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Conversation
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Exposure != want.Exposure {
		t.Fatalf("Exposure after JSON = %+v, want %+v", got.Exposure, want.Exposure)
	}
}

func TestMigrationV6(t *testing.T) {
	db := openTestDB(t)
	for range 2 { // the second run must be a no-op
		if err := runMigrations(t.Context(), db); err != nil {
			t.Fatal(err)
		}
	}
	assertMigrationVersions(t, db, "1,2,3,4,5,6")
	assertMigrationSQL(t, db, `SELECT group_concat(name || ':' || type || ':' || "notnull", ',') FROM (SELECT * FROM pragma_table_info('conversations') WHERE name LIKE 'exposure%' ORDER BY name)`,
		"exposure:TEXT:0,exposure_revision:INTEGER:0")
}
