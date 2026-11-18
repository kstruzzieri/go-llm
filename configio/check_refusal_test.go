package configio

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/brokenbody"
	"github.com/kstruzzieri/go-llm/provider"
)

// rfHook runs inside a store transaction when a trigger calls rf_hook().
var rfHook atomic.Pointer[func()]

func init() {
	// SQLite gives a function only to connections opened after it is
	// registered, so it is registered before any test opens a store.
	sqlite.MustRegisterScalarFunction("rf_hook", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if f := rfHook.Load(); f != nil {
			(*f)()
		}
		return int64(0), nil
	})
}

// rfOtherKey is a second credential at the same endpoint.
const rfOtherKey = "sk-other-test-key-91be"

// rfForbidden is a 403 whose body is read in full.
var rfForbidden = rcStatus(http.StatusForbidden, `{"error":{"message":"forbidden"}}`)

// rfInvalid is a completed tools stream whose one call names another tool:
// chat and stream yes, tool_call tool_call_invalid.
var rfInvalid = strings.Replace(rcCalled, `"get_time"`, `"not_get_time"`, 1)

// rfSSE serves body as an event stream.
func rfSSE(body string) rcServe {
	return func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

// rfByModel serves each named model its own way, and every other model a
// usable reply.
func rfByModel(serves map[string]rcServe) rcServe {
	return func(w http.ResponseWriter, r *http.Request, model string, n int) {
		if s, ok := serves[model]; ok {
			s(w, r, model, n)
			return
		}
		rcAnswer(w, r, model, n)
	}
}

// rfStore is a store at its own path and a second handle on the same file,
// for triggers and row counts.
func rfStore(t *testing.T) (*fingerprint.SQLiteStore, *sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ev.db")
	s := openStoreAt(t, path)
	u := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return s, db, path
}

// rfAccessRows is every model_access row as "model state status", sorted.
func rfAccessRows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT model_name, state, COALESCE(http_status, 0) FROM model_access`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var model, state string
		var status int
		if err := rows.Scan(&model, &state, &status); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %d", model, state, status))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// rfTrigger runs body before every insert of an access row in state.
func rfTrigger(t *testing.T, db *sql.DB, state, body string) {
	t.Helper()
	q := fmt.Sprintf(`CREATE TRIGGER rf_%s BEFORE INSERT ON model_access WHEN NEW.state = '%s' BEGIN %s; END`, state, state, body)
	if _, err := db.ExecContext(t.Context(), q); err != nil {
		t.Fatal(err)
	}
}

func rfAccess(t *testing.T, s fingerprint.EvidenceReader, b provider.EvidenceBinding, key, model string) *fingerprint.ModelAccess {
	t.Helper()
	a, err := s.LatestModelAccess(t.Context(), b.Endpoint, key, model)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func rfRows(t *testing.T, s fingerprint.EvidenceReader, b provider.EvidenceBinding, model string) string {
	t.Helper()
	rows, err := s.EvidenceRows(t.Context(), b.Endpoint, model)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%+v", rows)
}

// rfResult is model's one result.
func rfResult(t *testing.T, res []CheckResult, model string) CheckResult {
	t.Helper()
	var found []CheckResult
	for _, r := range res {
		if r.Model == model {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("results for %q: %+v; want exactly one among %+v", model, found, res)
	}
	return found[0]
}

// rfWant fails unless model's result has code, persisted and requests, and
// answers only when it has no code.
func rfWant(t *testing.T, res []CheckResult, model string, code ErrorCode, persisted bool, requests int) CheckResult {
	t.Helper()
	r := rfResult(t, res, model)
	if r.Code != code || r.Persisted != persisted || r.Requests != requests || (code == "") != (len(r.Answers) > 0) {
		t.Fatalf("result %+v; want code %q, persisted %v, %d requests", r, code, persisted, requests)
	}
	return r
}

// rfOrder records the order OnResult reported models in.
type rfOrder struct {
	mu     sync.Mutex
	models []string
}

func (o *rfOrder) on(r CheckResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.models = append(o.models, r.Model)
}

func (o *rfOrder) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.models)
}

// rfReadConn signals reads when its client reads response bytes.
type rfReadConn struct {
	net.Conn
	reads chan<- struct{}
}

func (c rfReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		select {
		case c.reads <- struct{}{}:
		default:
		}
	}
	return n, err
}

// rfReset is a transport whose connections report reads, and a 403 broken off
// before its cap: the status and part of the body, then, once the client has
// read them, a reset.
func rfReset(t *testing.T) (*http.Transport, rcServe) {
	reads := make(chan struct{}, 1)
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return rfReadConn{Conn: c, reads: reads}, nil
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return tr, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
		select { // a read of an earlier reply
		case <-reads:
		default:
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprint(buf, "HTTP/1.1 403 Forbidden\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"error\":\"cut")
		if err := buf.Flush(); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-reads:
		case <-time.After(5 * time.Second):
			t.Error("the client never read the refusal")
		}
		_ = conn.(*net.TCPConn).SetLinger(0)
	}
}

// One check runs at a time at a loopback endpoint, in model order, so in these
// tests a model sorted first is checked, and if refused held, before the next
// model's request is sent.

func TestRunCheck_RefusalHeldWhateverTheBodyRead(t *testing.T) {
	resetTransport, reset := rfReset(t)
	for _, tt := range []struct {
		name  string
		serve rcServe
		tr    *http.Transport
	}{
		{"read in full", rfForbidden, nil},
		{"cut past the 64 KiB cap", rcStatus(http.StatusForbidden, `{"error":{"message":"`+strings.Repeat("x", 100<<10)+`"}}`), nil},
		{"broken off short", func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
			if err := brokenbody.New().Write(w, http.StatusForbidden, false); err != nil {
				t.Error(err)
			}
		}, nil},
		{"reset before the cap", reset, resetTransport},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newRCStub(t, rfByModel(map[string]rcServe{"a": tt.serve}))
			cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
			b := cpBinding(t, cfg, "lc")
			store, db, _ := rfStore(t)
			plan := cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false)
			var order rfOrder
			res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store, Transport: tt.tr, OnResult: order.on})
			if err != nil {
				t.Fatal(err)
			}
			rfWant(t, res, "a", CodeModelRefused, true, 1)
			rfWant(t, res, "b", "", true, 1)
			if got := order.get(); !slices.Equal(got, []string{"b", "a"}) {
				t.Fatalf("reported %q; want a held until b answered", got)
			}
			if acc := rfAccess(t, store, b, cpKey, "a"); acc == nil || acc.State != fingerprint.AccessRefused || acc.HTTPStatus != 403 {
				t.Fatalf("a's access = %+v; want refused 403", acc)
			}
			if got := rfAccessRows(t, db); !slices.Equal(got, []string{"a refused 403", "b answered 0"}) {
				t.Fatalf("access rows %q", got)
			}
		})
	}
}

func TestRunCheck_403WhileAnotherAnswered_ModelRefusedSurvivesReopen(t *testing.T) {
	stub := newRCStub(t, rfByModel(map[string]rcServe{"b": rfForbidden}))
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
	bind := cpBinding(t, cfg, "lc")
	store, _, path := rfStore(t)
	cpAppend(t, store, cpYes(bind, "b", "chat", "stream", "tool_call")...)
	before := rfRows(t, store, bind, "b")
	plan := cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, true) // forced: b's answer is fresh
	res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	rfWant(t, res, "a", "", true, 1)
	rfWant(t, res, "b", CodeModelRefused, true, 1)

	reopened := openStoreAt(t, path)
	if acc := rfAccess(t, reopened, bind, cpKey, "b"); acc == nil || acc.State != fingerprint.AccessRefused || acc.HTTPStatus != 403 {
		t.Fatalf("b's access after reopening = %+v; want refused 403", acc)
	}
	if after := rfRows(t, reopened, bind, "b"); after != before {
		t.Fatalf("b's capability rows changed:\n%s\nwant\n%s", after, before)
	}
	if again := cpPlan(t, cfg, reopened, "lc", []string{"b"}, cpTools, false); len(again.Items) != 0 {
		t.Fatalf("planned %q after the refusal; want b's earlier answer still fresh", cpPlanned(again))
	}
}

func TestRunCheck_403ArrivesBeforeTheAnswer_StillModelRefused(t *testing.T) {
	for _, tt := range []struct {
		name, refused, answers string
	}{
		{"the refusal first", "a", "b"},
		{"the answer first", "b", "a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newRCStub(t, rfByModel(map[string]rcServe{tt.refused: rfForbidden}))
			cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
			store, db, _ := rfStore(t)
			plan := cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false)
			res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
			if err != nil {
				t.Fatal(err)
			}
			rfWant(t, res, tt.refused, CodeModelRefused, true, 1)
			rfWant(t, res, tt.answers, "", true, 1)
			want := []string{tt.answers + " answered 0", tt.refused + " refused 403"}
			slices.Sort(want)
			if got := rfAccessRows(t, db); !slices.Equal(got, want) {
				t.Fatalf("access rows %q; want %q", got, want)
			}
		})
	}

	// At a remote endpoint the checks run at once. r1 and r2 are refused before
	// the answer is sent and r3 after it; each is filed and reported once.
	t.Run("concurrent", func(t *testing.T) {
		var early sync.WaitGroup
		early.Add(2)
		answered := make(chan struct{})
		wait := func(ch <-chan struct{}) {
			select {
			case <-ch:
			case <-time.After(10 * time.Second):
				t.Error("barrier never opened")
			}
		}
		earlyDone := make(chan struct{})
		go func() { early.Wait(); close(earlyDone) }()
		refuseEarly := func(w http.ResponseWriter, r *http.Request, model string, n int) {
			defer early.Done()
			rfForbidden(w, r, model, n)
		}
		stub := newRCStub(t, rfByModel(map[string]rcServe{
			"r1": refuseEarly,
			"r2": refuseEarly,
			"ok": func(w http.ResponseWriter, r *http.Request, model string, n int) {
				defer close(answered)
				wait(earlyDone)
				rcAnswer(w, r, model, n)
			},
			"r3": func(w http.ResponseWriter, r *http.Request, model string, n int) {
				wait(answered)
				rfForbidden(w, r, model, n)
			},
		}))
		tr, grant, _ := rcRemote(t, stub, "rc")
		models := []string{"ok", "r1", "r2", "r3"}
		cfg := rcConfig("rc", rcRemoteBase, "openai-compat", cpKey, models...)
		store, db, _ := rfStore(t)
		plan := cpPlan(t, cfg, store, "rc", models, cpTools, false)
		res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store, Policy: grant, Transport: tr})
		if err != nil || len(res) != len(models) {
			t.Fatalf("RunCheck = %+v, %v; want one result per model", res, err)
		}
		rfWant(t, res, "ok", "", true, 1)
		for _, m := range []string{"r1", "r2", "r3"} {
			rfWant(t, res, m, CodeModelRefused, true, 1)
		}
		want := []string{"ok answered 0", "r1 refused 403", "r2 refused 403", "r3 refused 403"}
		if got := rfAccessRows(t, db); !slices.Equal(got, want) {
			t.Fatalf("access rows %q; want %q", got, want)
		}
	})
}

func TestRunCheck_OneModel403_RefusedNothingSaved(t *testing.T) {
	for _, format := range []string{"openai-compat", "ollama"} {
		for _, status := range []int{403, 404} {
			t.Run(fmt.Sprintf("%s %d", format, status), func(t *testing.T) {
				stub := newRCStub(t, rcStatus(status, `{"error":{"message":"no"}}`))
				cfg := rcConfig("p", stub.srv.URL, format, cpKey, "m")
				b := cpBinding(t, cfg, "p")
				store, db, _ := rfStore(t)
				plan := cpPlan(t, cfg, store, "p", []string{"m"}, cpTools, false)
				res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
				if err != nil || len(res) != 1 {
					t.Fatalf("RunCheck = %+v, %v; want one result", res, err)
				}
				rfWant(t, res, "m", CodeRefused, false, 1)
				if rows := rfRows(t, store, b, "m"); rows != "[]" {
					t.Fatalf("rows %s; want none", rows)
				}
				if got := rfAccessRows(t, db); len(got) != 0 {
					t.Fatalf("access rows %q; want none", got)
				}
			})
		}
	}
}

func TestRunCheck_401_AuthFailed(t *testing.T) {
	stub := newRCStub(t, rfByModel(map[string]rcServe{"b": rcStatus(http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)}))
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
	store, db, _ := rfStore(t)
	plan := cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false)
	res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	rfWant(t, res, "a", "", true, 1)
	rfWant(t, res, "b", CodeAuthFailed, false, 1)
	if got := rfAccessRows(t, db); !slices.Equal(got, []string{"a answered 0"}) {
		t.Fatalf("access rows %q; want only a's answer", got)
	}
}

func TestRunCheck_AnsweredRowWritten(t *testing.T) {
	for _, tt := range []struct {
		name, format string
		caps         provider.Capability
		answered     bool
	}{
		{"chat-stream-tools", "openai-compat", cpTools, true},
		{"chat-stream", "openai-compat", provider.CapChat | provider.CapStream, true},
		{"ollama-show", "ollama", cpTools, false}, // metadata: its facts only
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newRCStub(t, nil)
			cfg := rcConfig("p", stub.srv.URL, tt.format, cpKey, "m")
			b := cpBinding(t, cfg, "p")
			store, db, _ := rfStore(t)
			plan := cpPlan(t, cfg, store, "p", []string{"m"}, tt.caps, false)
			res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
			if err != nil {
				t.Fatal(err)
			}
			if r := rfWant(t, res, "m", "", true, 1); r.Method != tt.name {
				t.Fatalf("method %q; want %q", r.Method, tt.name)
			}
			rows, err := store.EvidenceRows(t.Context(), b.Endpoint, "m")
			if err != nil || len(rows) == 0 {
				t.Fatalf("rows %d (%v); want the answers saved", len(rows), err)
			}
			acc := rfAccess(t, store, b, cpKey, "m")
			if !tt.answered {
				if acc != nil || len(rfAccessRows(t, db)) != 0 {
					t.Fatalf("access %+v; want none for a metadata answer", acc)
				}
				return
			}
			if acc == nil || acc.State != fingerprint.AccessAnswered || acc.HTTPStatus != 0 ||
				!acc.ObservedAt.Equal(rows[0].ObservedAt) || !acc.ExpiresAt.IsZero() {
				t.Fatalf("access %+v; want answered, no status, observed with the rows at %v, no expiry", acc, rows[0].ObservedAt)
			}
		})
	}
}

func TestRunCheck_TwoKeysRefusalsSeparate(t *testing.T) {
	stub := newRCStub(t, rfByModel(map[string]rcServe{"b": rfForbidden}))
	cfgA := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
	cfgB := rcConfig("lc", stub.srv.URL, "openai-compat", rfOtherKey, "a", "b")
	bind := cpBinding(t, cfgA, "lc")
	store, _, _ := rfStore(t)

	res, err := rcRun(t, cpPlan(t, cfgA, store, "lc", []string{"a", "b"}, cpTools, false), RunOptions{Config: cfgA, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	rfWant(t, res, "b", CodeModelRefused, true, 1)
	if acc := rfAccess(t, store, bind, rfOtherKey, "b"); acc != nil {
		t.Fatalf("the other key's access to b = %+v; want none", acc)
	}

	// Under the other key no model answers in the run, so b is refused, and
	// the first key's answer, in another run, corroborates nothing.
	res, err = rcRun(t, cpPlan(t, cfgB, store, "lc", []string{"b"}, cpTools, false), RunOptions{Config: cfgB, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	rfWant(t, res, "b", CodeRefused, false, 1)
	if acc := rfAccess(t, store, bind, rfOtherKey, "b"); acc != nil {
		t.Fatalf("the other key's access to b = %+v; want none", acc)
	}
	if acc := rfAccess(t, store, bind, cpKey, "b"); acc == nil || acc.State != fingerprint.AccessRefused {
		t.Fatalf("the first key's access to b = %+v; want its refusal", acc)
	}
}

func TestRunCheck_ListingUnderOneKeyNeverMakesOtherModelRefused(t *testing.T) {
	stub := newRCStub(t, rfByModel(map[string]rcServe{"b": rfForbidden}))
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
	bind := cpBinding(t, cfg, "lc")
	store, db, _ := rfStore(t)
	cpList(t, store, bind, cpKey, "a", "b")
	cpList(t, store, bind, rfOtherKey, "a", "b")
	res, err := rcRun(t, cpPlan(t, cfg, store, "lc", []string{"b"}, cpTools, false), RunOptions{Config: cfg, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	rfWant(t, res, "b", CodeRefused, false, 1)
	if got := rfAccessRows(t, db); len(got) != 0 {
		t.Fatalf("access rows %q; want none", got)
	}
}

func TestRunCheck_RequestRejectedDoesNotCorroborate(t *testing.T) {
	stub := newRCStub(t, rfByModel(map[string]rcServe{
		"a": rcStatus(http.StatusBadRequest, `{"error":{"message":"unknown model"}}`), // as some gateways answer
		"b": rfForbidden,
	}))
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
	bind := cpBinding(t, cfg, "lc")
	store, db, _ := rfStore(t)
	res, err := rcRun(t, cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false), RunOptions{Config: cfg, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	a := rfWant(t, res, "a", "", true, 2)
	want := []CheckAnswer{{Capability: "tool_call", State: "inconclusive", Detail: fingerprint.DetailRequestRejected}}
	if !slices.Equal(a.Answers, want) {
		t.Fatalf("a's answers %+v; want %+v", a.Answers, want)
	}
	if rows, _ := store.EvidenceRows(t.Context(), bind.Endpoint, "a"); len(rows) != 1 || rows[0].Detail != fingerprint.DetailRequestRejected {
		t.Fatalf("a's rows %+v; want its request_rejected attempt", rows)
	}
	rfWant(t, res, "b", CodeRefused, false, 1)
	if got := rfAccessRows(t, db); len(got) != 0 {
		t.Fatalf("access rows %q; want none", got)
	}
}

func TestRunCheck_OllamaShowNeverCorroborates(t *testing.T) {
	stub := newRCStub(t, rfByModel(map[string]rcServe{"b": rcStatus(http.StatusNotFound, `{"error":"model not found"}`)}))
	cfg := rcConfig("ol", stub.srv.URL, "ollama", cpKey, "a", "b")
	bind := cpBinding(t, cfg, "ol")
	store, db, _ := rfStore(t)
	res, err := rcRun(t, cpPlan(t, cfg, store, "ol", []string{"a", "b"}, cpTools, false), RunOptions{Config: cfg, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	a := rfWant(t, res, "a", "", true, 1)
	if !slices.Contains(a.Answers, CheckAnswer{Capability: "chat", State: "yes"}) {
		t.Fatalf("a's answers %+v; want chat yes, so only the method keeps it from corroborating", a.Answers)
	}
	if rows := rfRows(t, store, bind, "a"); rows == "[]" {
		t.Fatal("a's facts were not saved")
	}
	rfWant(t, res, "b", CodeRefused, false, 1)
	if got := rfAccessRows(t, db); len(got) != 0 {
		t.Fatalf("access rows %q; want none", got)
	}
}

func TestRunCheck_AccessRowStatusAndTime(t *testing.T) {
	for _, status := range []int{403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var answerSent atomic.Int64 // when b's request reached the stub, which then waits
			stub := newRCStub(t, rfByModel(map[string]rcServe{
				"a": rcStatus(status, `{"error":{"message":"no"}}`),
				"b": func(w http.ResponseWriter, r *http.Request, model string, n int) {
					answerSent.Store(time.Now().UnixNano())
					time.Sleep(300 * time.Millisecond)
					rcAnswer(w, r, model, n)
				},
			}))
			cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
			bind := cpBinding(t, cfg, "lc")
			store, _, _ := rfStore(t)
			plan := cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false)
			start := time.Now().Truncate(time.Millisecond)
			res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
			if err != nil {
				t.Fatal(err)
			}
			rfWant(t, res, "a", CodeModelRefused, true, 1)
			acc := rfAccess(t, store, bind, cpKey, "a")
			sent := time.Unix(0, answerSent.Load())
			if acc == nil || acc.State != fingerprint.AccessRefused || acc.HTTPStatus != status ||
				acc.ObservedAt.Before(start) || !acc.ObservedAt.Before(sent) || !acc.ExpiresAt.Equal(acc.ObservedAt.Add(7*24*time.Hour)) {
				t.Fatalf("a's access %+v; want refused %d, observed between %v and b's request at %v, expiring 7 days later", acc, status, start, sent)
			}
		})
	}
}

func TestRunCheck_InvalidToolCallSavesAttemptAndAnswered(t *testing.T) {
	for _, tt := range []struct {
		name     string
		serve    rcServe
		requests int
	}{
		{"on the first request", rfSSE(rfInvalid), 1},
		{"on the second request", func(w http.ResponseWriter, r *http.Request, model string, n int) {
			if n == 0 {
				rcStatus(http.StatusBadRequest, `{"error":{"message":"tool_choice"}}`)(w, r, model, n)
				return
			}
			rfSSE(rfInvalid)(w, r, model, n)
		}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newRCStub(t, rfByModel(map[string]rcServe{"a": rfForbidden, "b": tt.serve}))
			cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
			bind := cpBinding(t, cfg, "lc")
			store, _, _ := rfStore(t)
			var order rfOrder
			res, err := rcRun(t, cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false), RunOptions{Config: cfg, Store: store, OnResult: order.on})
			if err != nil {
				t.Fatal(err)
			}
			b := rfWant(t, res, "b", "", true, tt.requests)
			want := []CheckAnswer{{Capability: "chat", State: "yes"}, {Capability: "stream", State: "yes"},
				{Capability: "tool_call", State: "inconclusive", Detail: fingerprint.DetailToolCallInvalid}}
			if !slices.Equal(b.Answers, want) {
				t.Fatalf("b's answers %+v; want %+v", b.Answers, want)
			}
			rows, err := store.EvidenceRows(t.Context(), bind.Endpoint, "b")
			if err != nil || len(rows) != 3 {
				t.Fatalf("b's rows %+v (%v); want three", rows, err)
			}
			if acc := rfAccess(t, store, bind, cpKey, "b"); acc == nil || acc.State != fingerprint.AccessAnswered || !acc.ObservedAt.Equal(rows[0].ObservedAt) {
				t.Fatalf("b's access %+v; want answered with its rows", acc)
			}
			rfWant(t, res, "a", CodeModelRefused, true, 1)
			if got := order.get(); !slices.Equal(got, []string{"b", "a"}) {
				t.Fatalf("reported %q; want a held until b answered", got)
			}
		})
	}
}

func TestRunCheck_OversizedResponseSavesNeitherAnswerNorAccess(t *testing.T) {
	stub := newRCStub(t, rfByModel(map[string]rcServe{
		"a": func(w http.ResponseWriter, r *http.Request, _ string, _ int) {
			w.Header().Set("Content-Type", "text/event-stream")
			chunk := rcFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": strings.Repeat("x", 4096)}}}})
			for r.Context().Err() == nil {
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
			}
		},
		"b": rfForbidden,
	}))
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
	bind := cpBinding(t, cfg, "lc")
	store, db, _ := rfStore(t)
	res, err := rcRun(t, cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false), RunOptions{Config: cfg, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	rfWant(t, res, "a", CodeResponseTooLarge, false, 1)
	rfWant(t, res, "b", CodeRefused, false, 1)
	if rows := rfRows(t, store, bind, "a"); rows != "[]" {
		t.Fatalf("a's rows %s; want none", rows)
	}
	if got := rfAccessRows(t, db); len(got) != 0 {
		t.Fatalf("access rows %q; want none", got)
	}
}

func TestRunCheck_ForcedExplicitModelSendsNoToolRequest(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey)
	cfg.Models["lc-00"] = config.ModelConfig{Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}}
	plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, true)
	if len(plan.Items) != 0 {
		t.Fatalf("planned %q; want an explicit model never planned", cpPlanned(plan))
	}
	res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: newStore(t)})
	if err != nil || len(res) != 0 || stub.sum() != 0 {
		t.Fatalf("RunCheck = %+v, %v, server received %d; want nothing sent", res, err, stub.sum())
	}
}

func TestRunCheck_NoRawProviderText(t *testing.T) {
	const marker = "SECRET-MARKER"
	body := `{"error":{"message":"` + marker + `"}}`
	for _, tt := range []struct {
		name   string
		serves map[string]rcServe
		models []string
		want   ErrorCode
	}{
		{"server error", map[string]rcServe{"m": rcStatus(500, body)}, []string{"m"}, CodeServerError},
		{"auth failed", map[string]rcServe{"m": rcStatus(401, body)}, []string{"m"}, CodeAuthFailed},
		{"refused", map[string]rcServe{"m": rcStatus(403, body)}, []string{"m"}, CodeRefused},
		{"model refused", map[string]rcServe{"m": rcStatus(404, body)}, []string{"a", "m"}, CodeModelRefused},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newRCStub(t, rfByModel(tt.serves))
			cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, tt.models...)
			store := newStore(t)
			res, err := rcRun(t, cpPlan(t, cfg, store, "lc", tt.models, cpTools, false), RunOptions{Config: cfg, Store: store})
			if err != nil {
				t.Fatal(err)
			}
			if r := rfResult(t, res, "m"); r.Code != tt.want {
				t.Fatalf("result %+v; want %q", r, tt.want)
			}
			shown, _ := json.Marshal(res)
			if text := fmt.Sprintf("%+v %s %v", res, shown, err); strings.Contains(text, marker) {
				t.Fatalf("results carry provider text: %s", text)
			}
		})
	}
}

func TestRunCheck_FailureCodesTable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	_ = ln.Close()
	stall := func(_ http.ResponseWriter, r *http.Request, _ string, _ int) { <-r.Context().Done() }
	for _, tt := range []struct {
		name     string
		serve    rcServe
		base     string // "" is the stub
		timeout  time.Duration
		want     ErrorCode
		requests int
	}{
		{"429", rcStatus(429, `{"error":{"message":"slow down"}}`), "", 0, CodeRateLimited, 1},
		{"a timeout", stall, "", 300 * time.Millisecond, CodeTimeout, 1},
		{"a refused connection", nil, closed, 0, CodeUnreachable, 0},
		{"503", rcStatus(503, `{"error":{"message":"down"}}`), "", 0, CodeServerError, 1},
		{"another 4xx", rcStatus(409, `{"error":{"message":"conflict"}}`), "", 0, CodeRejected, 1},
		{"a bad stream", rfSSE("data: {not json\n\n"), "", 0, CodeMalformedResponse, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newRCStub(t, tt.serve)
			cfg := rcConfig("lc", cmp.Or(tt.base, stub.srv.URL), "openai-compat", cpKey, "m")
			pc := cfg.Providers["lc"]
			pc.Timeout.Duration = tt.timeout
			cfg.Providers["lc"] = pc
			bind := cpBinding(t, cfg, "lc")
			store, db, _ := rfStore(t)
			res, err := rcRun(t, cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false), RunOptions{Config: cfg, Store: store})
			if err != nil {
				t.Fatal(err)
			}
			rfWant(t, res, "m", tt.want, false, tt.requests)
			if rows := rfRows(t, store, bind, "m"); rows != "[]" || len(rfAccessRows(t, db)) != 0 {
				t.Fatalf("rows %s, access %q; want nothing saved", rows, rfAccessRows(t, db))
			}
		})
	}
}

func TestRunCheck_StoreDisabledPersistedFalse(t *testing.T) {
	stub := newRCStub(t, rfByModel(map[string]rcServe{"b": rfForbidden}))
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
	res, err := rcRun(t, cpPlan(t, cfg, nil, "lc", []string{"a", "b"}, cpTools, false), RunOptions{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	rfWant(t, res, "a", "", false, 1)
	rfWant(t, res, "b", CodeModelRefused, false, 1)
}

func TestRunCheck_AnswersAndAccessCommitTogether(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	bind := cpBinding(t, cfg, "lc")
	store, db, _ := rfStore(t)
	cpAppend(t, store, cpYes(bind, "m", "chat", "stream", "tool_call")...)
	if err := store.RecordModelAccess(t.Context(), bind.Endpoint, cpKey, "m", fingerprint.AccessRefused, 403, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := rfRows(t, store, bind, "m")
	rfTrigger(t, db, "answered", `SELECT RAISE(ABORT, 'SECRET-DB-TRIGGER')`)
	res, err := rcRun(t, cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, true), RunOptions{Config: cfg, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	r := rfWant(t, res, "m", "", false, 1)
	if shown, _ := json.Marshal(r); strings.Contains(fmt.Sprintf("%+v %s", r, shown), "SECRET-DB-TRIGGER") {
		t.Fatalf("result carries the store's text: %+v", r)
	}
	if after := rfRows(t, store, bind, "m"); after != before {
		t.Fatalf("rows changed when the access write failed:\n%s\nwant\n%s", after, before)
	}
	if acc := rfAccess(t, store, bind, cpKey, "m"); acc == nil || acc.State != fingerprint.AccessRefused {
		t.Fatalf("access %+v; want the earlier refusal", acc)
	}
}

func TestRunCheck_RefusalWriteFailurePersistedFalse(t *testing.T) {
	stub := newRCStub(t, rfByModel(map[string]rcServe{"b": rfForbidden}))
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
	store, db, _ := rfStore(t)
	rfTrigger(t, db, "refused", `SELECT RAISE(ABORT, 'SECRET-DB-TRIGGER')`)
	res, err := rcRun(t, cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false), RunOptions{Config: cfg, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	rfWant(t, res, "a", "", true, 1)
	rfWant(t, res, "b", CodeModelRefused, false, 1)
	if got := rfAccessRows(t, db); !slices.Equal(got, []string{"a answered 0"}) {
		t.Fatalf("access rows %q; want only a's answer", got)
	}
}

func TestRunCheck_MemoryOnlyStatusIsNotCalledDurable(t *testing.T) {
	dir := t.TempDir()
	open := func() *fingerprint.SharedStore {
		// The data directory inside the workspace: the store degrades to memory.
		s, err := fingerprint.OpenSharedStore(t.Context(), func(k string) string {
			return map[string]string{"XDG_DATA_HOME": dir, "HOME": dir}[k]
		}, dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if s.Status() != fingerprint.StoreMemoryOnly {
			t.Fatalf("store status %q; want memory-only", s.Status())
		}
		return s
	}
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	bind := cpBinding(t, cfg, "lc")
	shared := open()
	res, err := rcRun(t, cpPlan(t, cfg, shared.Log(), "lc", []string{"m"}, cpTools, false), RunOptions{Config: cfg, Store: shared.Log()})
	if err != nil {
		t.Fatal(err)
	}
	// Persisted says the answers were committed to the store it was given; the
	// store's status says whether that outlives the process.
	rfWant(t, res, "m", "", true, 1)
	if rows := rfRows(t, shared.Log(), bind, "m"); rows == "[]" {
		t.Fatal("the answers are not in the store they were committed to")
	}
	if rows := rfRows(t, open().Log(), bind, "m"); rows != "[]" {
		t.Fatalf("a second memory-only store holds %s; want nothing", rows)
	}
}

// rfCancelAfterAppend cancels the run once a check's append has returned.
type rfCancelAfterAppend struct {
	fingerprint.EvidenceStore
	cancel context.CancelFunc
}

func (s rfCancelAfterAppend) AppendCheckResult(ctx context.Context, endpoint, apiKey, model string, rows []fingerprint.EvidenceRow, access *fingerprint.ModelAccess) error {
	defer s.cancel()
	return s.EvidenceStore.AppendCheckResult(ctx, endpoint, apiKey, model, rows, access)
}

func TestRunCheck_CommitBeforeCancellationStaysSaved(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
	bind := cpBinding(t, cfg, "lc")
	store, db, _ := rfStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	plan := cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false)
	res, err := rcRunCtx(t, ctx, plan, RunOptions{Config: cfg, Store: rfCancelAfterAppend{store, cancel}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCheck error = %v; want the cancellation", err)
	}
	if len(res) != 1 {
		t.Fatalf("results %+v; want a's alone", res)
	}
	rfWant(t, res, "a", "", true, 1)
	if rows := rfRows(t, store, bind, "a"); rows == "[]" {
		t.Fatal("a's committed answers are gone")
	}
	if got := rfAccessRows(t, db); !slices.Equal(got, []string{"a answered 0"}) {
		t.Fatalf("access rows %q; want a's answer", got)
	}
	if stub.count("b") != 0 {
		t.Fatalf("server received %d requests for b after the cancellation", stub.count("b"))
	}
}

func TestRunCheck_CancelBetweenWritesRollsBackWholeResult(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	bind := cpBinding(t, cfg, "lc")
	store, db, _ := rfStore(t)
	cpAppend(t, store, cpYes(bind, "m", "chat", "stream", "tool_call")...)
	before := rfRows(t, store, bind, "m")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var hooked atomic.Int32
	hook := func() { hooked.Add(1); cancel() }
	rfHook.Store(&hook)
	t.Cleanup(func() { rfHook.Store(nil) })
	rfTrigger(t, db, "answered", `SELECT rf_hook()`) // after the evidence rows, inside the transaction
	res, err := rcRunCtx(t, ctx, cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, true), RunOptions{Config: cfg, Store: store})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCheck error = %v; want the cancellation", err)
	}
	if hooked.Load() != 1 || len(res) != 0 {
		t.Fatalf("hook ran %d times, results %+v; want once and none", hooked.Load(), res)
	}
	if after := rfRows(t, store, bind, "m"); after != before {
		t.Fatalf("rows changed:\n%s\nwant\n%s", after, before)
	}
	if got := rfAccessRows(t, db); len(got) != 0 {
		t.Fatalf("access rows %q; want none", got)
	}
}

func TestRunCheck_CancelledFromOnUsage(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	bind := cpBinding(t, cfg, "lc")
	store, db, _ := rfStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var usage atomic.Int32
	res, err := rcRunCtx(t, ctx, cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false), RunOptions{
		Config: cfg, Store: store,
		OnUsage: func(string, provider.Usage) { usage.Add(1); cancel() }, // the stream completed
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCheck error = %v; want the cancellation", err)
	}
	if code, ok := CodeOf(err); ok {
		t.Fatalf("error carries code %q; want the raw context error", code)
	}
	if usage.Load() != 1 || len(res) != 0 {
		t.Fatalf("usage callbacks %d, results %+v; want one and none", usage.Load(), res)
	}
	if rows := rfRows(t, store, bind, "m"); rows != "[]" || len(rfAccessRows(t, db)) != 0 {
		t.Fatalf("rows %s, access %q; want nothing saved", rows, rfAccessRows(t, db))
	}
}

func TestRunCheck_AlreadyCancelledSendsNothing(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var usage atomic.Int32
	res, err := rcRunCtx(t, ctx, plan, RunOptions{Config: cfg, Store: newStore(t), OnUsage: func(string, provider.Usage) { usage.Add(1) }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCheck error = %v; want the cancellation", err)
	}
	if code, ok := CodeOf(err); ok || len(res) != 0 || stub.sum() != 0 || usage.Load() != 0 {
		t.Fatalf("code %q (%v), results %+v, server received %d, usage %d; want the raw error and nothing sent", code, ok, res, stub.sum(), usage.Load())
	}
}

func TestRunCheck_CancelledForceLeavesEarlierAnswer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stub := newRCStub(t, func(_ http.ResponseWriter, r *http.Request, _ string, _ int) {
		cancel() // while the check is in flight
		<-r.Context().Done()
	})
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	bind := cpBinding(t, cfg, "lc")
	store, db, _ := rfStore(t)
	cpAppend(t, store, cpYes(bind, "m", "chat", "stream", "tool_call")...)
	before := rfRows(t, store, bind, "m")
	res, err := rcRunCtx(t, ctx, cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, true), RunOptions{Config: cfg, Store: store})
	if !errors.Is(err, context.Canceled) || len(res) != 0 || stub.count("m") != 1 {
		t.Fatalf("RunCheck = %+v, %v, server received %d; want the cancellation, no result, one request", res, err, stub.count("m"))
	}
	if after := rfRows(t, store, bind, "m"); after != before || len(rfAccessRows(t, db)) != 0 {
		t.Fatalf("rows changed or access written:\n%s\nwant\n%s", after, before)
	}
	if again := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false); len(again.Items) != 0 {
		t.Fatalf("planned %q; want the earlier answer still the reading", cpPlanned(again))
	}
}

func TestRunCheck_CancelStopsNewRequestsFinishedStaySaved(t *testing.T) {
	t.Run("one at a time", func(t *testing.T) {
		stub := newRCStub(t, nil)
		models := []string{"a", "b", "c"}
		cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, models...)
		bind := cpBinding(t, cfg, "lc")
		store := newStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		res, err := rcRunCtx(t, ctx, cpPlan(t, cfg, store, "lc", models, cpTools, false), RunOptions{
			Config: cfg, Store: store, OnResult: func(CheckResult) { cancel() },
		})
		if !errors.Is(err, context.Canceled) || len(res) != 1 {
			t.Fatalf("RunCheck = %+v, %v; want a's result and the cancellation", res, err)
		}
		rfWant(t, res, "a", "", true, 1)
		if stub.count("a") != 1 || stub.count("b") != 0 || stub.count("c") != 0 {
			t.Fatalf("server received a=%d b=%d c=%d; want 1, 0, 0", stub.count("a"), stub.count("b"), stub.count("c"))
		}
		if rfRows(t, store, bind, "a") == "[]" {
			t.Fatal("a's answers were not kept")
		}
	})
	t.Run("at once", func(t *testing.T) {
		stub := newRCStub(t, rfByModel(map[string]rcServe{
			"b": func(_ http.ResponseWriter, r *http.Request, _ string, _ int) { <-r.Context().Done() },
		}))
		tr, grant, _ := rcRemote(t, stub, "rc")
		cfg := rcConfig("rc", rcRemoteBase, "openai-compat", cpKey, "a", "b")
		bind := cpBinding(t, cfg, "rc")
		store, db, _ := rfStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		res, err := rcRunCtx(t, ctx, cpPlan(t, cfg, store, "rc", []string{"a", "b"}, cpTools, false), RunOptions{
			Config: cfg, Store: store, Policy: grant, Transport: tr, OnResult: func(CheckResult) { cancel() },
		})
		if !errors.Is(err, context.Canceled) || len(res) != 1 {
			t.Fatalf("RunCheck = %+v, %v; want a's result and the cancellation", res, err)
		}
		rfWant(t, res, "a", "", true, 1)
		if rfRows(t, store, bind, "a") == "[]" || rfRows(t, store, bind, "b") != "[]" {
			t.Fatal("want a's answers kept and none for b")
		}
		if got := rfAccessRows(t, db); !slices.Equal(got, []string{"a answered 0"}) {
			t.Fatalf("access rows %q; want a's answer", got)
		}
	})
}

// TestRunCheck_CancelDropsHeldRefusals: a refusal held when the caller cancels
// is neither saved nor reported, whether nothing had answered yet or the answer
// that would corroborate it was the last thing reported.
func TestRunCheck_CancelDropsHeldRefusals(t *testing.T) {
	t.Run("cancelled before any answer", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stub := newRCStub(t, rfByModel(map[string]rcServe{
			"a": rfForbidden,
			"b": func(_ http.ResponseWriter, r *http.Request, _ string, _ int) {
				cancel() // a is held by now
				<-r.Context().Done()
			},
		}))
		cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
		store, db, _ := rfStore(t)
		res, err := rcRunCtx(t, ctx, cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false), RunOptions{Config: cfg, Store: store})
		if !errors.Is(err, context.Canceled) || len(res) != 0 {
			t.Fatalf("RunCheck = %+v, %v; want no results and the cancellation", res, err)
		}
		if got := rfAccessRows(t, db); len(got) != 0 {
			t.Fatalf("access rows %q; want none", got)
		}
	})
	t.Run("cancelled from the corroborating answer's report", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stub := newRCStub(t, rfByModel(map[string]rcServe{"a": rfForbidden}))
		cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b")
		store, db, _ := rfStore(t)
		res, err := rcRunCtx(t, ctx, cpPlan(t, cfg, store, "lc", []string{"a", "b"}, cpTools, false), RunOptions{
			Config: cfg, Store: store,
			OnResult: func(r CheckResult) {
				if r.Model == "b" {
					cancel()
				}
			},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunCheck error = %v; want the cancellation", err)
		}
		if len(res) != 1 {
			t.Fatalf("results %+v; want b's alone", res)
		}
		rfWant(t, res, "b", "", true, 1)
		if got := rfAccessRows(t, db); !slices.Equal(got, []string{"b answered 0"}) {
			t.Fatalf("access rows %q; want b's answer alone", got)
		}
	})
}

// TestRunCheck_CancelFromOnResultStopsRefusalReports: nothing answers, so both
// refusals are reported once every check has finished; a cancel from the first
// report stops the second.
func TestRunCheck_CancelFromOnResultStopsRefusalReports(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stub := newRCStub(t, rfByModel(map[string]rcServe{
		"a": rfForbidden,
		"c": rcStatus(http.StatusNotFound, `{"error":{"message":"no such model"}}`),
	}))
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "c")
	store, db, _ := rfStore(t)
	res, err := rcRunCtx(t, ctx, cpPlan(t, cfg, store, "lc", []string{"a", "c"}, cpTools, false), RunOptions{
		Config: cfg, Store: store,
		OnResult: func(r CheckResult) {
			if r.Model == "a" {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) || len(res) != 1 {
		t.Fatalf("RunCheck = %+v, %v; want a's result alone and the cancellation", res, err)
	}
	rfWant(t, res, "a", CodeRefused, false, 1)
	if got := rfAccessRows(t, db); len(got) != 0 {
		t.Fatalf("access rows %q; want none", got)
	}
}
