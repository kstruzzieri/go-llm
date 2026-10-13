package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// replayAttemptSchema versions attempt.json (#687). Any change to its
// fields or their meaning is a new version.
const replayAttemptSchema = "llm-bench.replay-attempt/v1"

const (
	attemptStatusCompleted = "completed"
	attemptStatusFailed    = "failed"
)

// attemptIDPattern keeps X-Client-Correlation ASCII, untrimmed and at most
// 149 bytes (128 + "/" + a 20-digit ordinal), under the lab gateway's 256.
var attemptIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// replayAttemptOptions are the -replay-attempt inputs. publisher and goos are
// test seams (file-operation failures, the windows refusal of decision W);
// their zero values are production behavior.
type replayAttemptOptions struct {
	AttemptDir string
	AttemptID  string
	Arm        string
	TracePath  string
	Models     string
	BaseURL    string
	APIKey     string
	Timeout    time.Duration

	publisher *attemptPublisher // nil: newAttemptPublisher(AttemptDir)
	goos      string            // "": runtime.GOOS
}

// attemptRecord is attempt.json, schema llm-bench.replay-attempt/v1.
type attemptRecord struct {
	Schema              string              `json:"schema"`
	AttemptID           string              `json:"attempt_id"`
	Arm                 string              `json:"arm"`
	Trace               attemptTraceRef     `json:"trace"`
	ReplayKind          *string             `json:"replay_kind"` // scripted | prefilled; null until the trace validates
	Build               attemptBuild        `json:"build"`
	Settings            attemptSettings     `json:"settings"`
	StartedAt           time.Time           `json:"started_at"`
	EndedAt             time.Time           `json:"ended_at"`
	ElapsedMs           int64               `json:"elapsed_ms"`
	Status              string              `json:"status"`
	Error               *attemptError       `json:"error"`
	ArtifactError       *attemptError       `json:"artifact_error"`
	Transcript          []Turn              `json:"transcript"`
	Notes               []string            `json:"notes"`
	Divergences         []string            `json:"divergences"`
	SentinelSubstituted bool                `json:"sentinel_substituted"`
	Requests            []attemptRequestRow `json:"requests"`
}

// attemptTraceRef identifies the replayed trace; SHA256 hashes the file
// bytes exactly as read (the same bytes that were then parsed). Each field is
// null until the run learns it, so an input failure never publishes an empty
// string where the schema promises an id or a hex digest.
type attemptTraceRef struct {
	ID     *string `json:"id"`
	SHA256 *string `json:"sha256"`
}

// attemptBuild identifies the llm-bench binary. go_version is always set. The
// other three are null when the binary does not report them: module_version
// when build info is unavailable or the main module has no version (Go's
// "(devel)" is kept verbatim), vcs_revision and vcs_modified when the build
// was not VCS-stamped.
type attemptBuild struct {
	GoVersion     string  `json:"go_version"`
	ModuleVersion *string `json:"module_version"`
	VCSRevision   *string `json:"vcs_revision"`
	VCSModified   *bool   `json:"vcs_modified"`
}

// attemptSettings is what the attempt ran with. Only two fields can be null:
// endpoint, until the base URL validates, and temperature, unless the replay
// sent one (prefilled replays send 0).
type attemptSettings struct {
	Transport   string   `json:"transport"`
	Endpoint    *string  `json:"endpoint"` // null until the base URL validates
	Model       string   `json:"model"`
	Temperature *float64 `json:"temperature"`
	TimeoutMs   int64    `json:"timeout_ms"`
}

// attemptRequestRow is one logical round trip. A transport-internal resend
// shares its correlation value, so one row can join more than one gateway
// receipt.
type attemptRequestRow struct {
	Ordinal     int                 `json:"ordinal"`
	Correlation string              `json:"correlation"`
	StartedAt   time.Time           `json:"started_at"`
	HeadersAt   *time.Time          `json:"headers_at"`
	EndedAt     time.Time           `json:"ended_at"`
	ElapsedMs   int64               `json:"elapsed_ms"`
	HTTPStatus  *int                `json:"http_status"`
	Request     *attemptFileRef     `json:"request"`
	Response    *attemptResponseRef `json:"response"`
	Usage       *attemptUsage       `json:"usage"`
	ErrorCode   *string             `json:"error_code"`
}

// runReplayAttempt runs exactly one replay attempt and publishes its attempt
// directory. It returns the process exit code (D3): 0 once attempt.json is
// published and synced, whatever the attempt's status; 1 when no durable
// terminal exists.
func runReplayAttempt(parent context.Context, opts replayAttemptOptions, stderr io.Writer) int {
	goos := opts.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos == "windows" {
		_, _ = fmt.Fprintln(stderr, "llm-bench: -replay-attempt is not supported on windows: it needs atomic rename and directory fsync")
		return 1
	}
	pub := opts.publisher
	if pub == nil {
		pub = newAttemptPublisher(opts.AttemptDir)
	}
	if err := pub.claim(); err != nil {
		reportAttemptDetail(stderr, err, opts.APIKey)
		return 1
	}
	if opts.APIKey != "" && !redactableKey(opts.APIKey) {
		_, _ = fmt.Fprintf(stderr, "llm-bench: replay-attempt: candidate key is shorter than %d bytes; evidence is not redacted\n", minRedactableKeyLen)
	}
	start := time.Now()
	rec := attemptRecord{
		Schema:    replayAttemptSchema,
		AttemptID: opts.AttemptID,
		Arm:       opts.Arm,
		Build:     readAttemptBuild(),
		Settings:  attemptSettings{Transport: openAICompatTransport, Model: strings.TrimSpace(opts.Models), TimeoutMs: opts.Timeout.Milliseconds()},
		StartedAt: start.UTC(),
	}
	var out replayOutput
	var rows []*recordedRequest
	trace, target, err := loadAttemptInputs(opts, &rec)
	if err != nil {
		reportAttemptDetail(stderr, err, opts.APIKey)
		rec.Error = &attemptError{Code: attemptCodeInput, Stage: attemptStageInput, Message: redactErrorMessage(err.Error())}
	} else {
		out, rows, rec.Error = executeAttempt(parent, opts, trace, target, stderr)
	}
	end := time.Now()
	rec.EndedAt = end.UTC()
	rec.ElapsedMs = end.Sub(start).Milliseconds()
	rec.Transcript = append([]Turn{}, out.Transcript...)
	rec.Notes = append([]string{}, out.Notes...)
	rec.Divergences = append([]string{}, out.Divergences...)
	rec.SentinelSubstituted = out.SentinelSubstituted

	refs, artifactErr, causes := pub.writeRaw(rows, opts.APIKey)
	// attempt.json carries only the category; the causes are supervisor
	// evidence (A4). causes is non-empty exactly when artifactErr is set.
	for _, cause := range causes {
		reportAttemptDetail(stderr, cause, opts.APIKey)
	}
	rec.Requests = buildAttemptRequestRows(rows, refs)
	rec.ArtifactError = artifactErr
	rec.Status = attemptStatusCompleted
	if rec.Error != nil || rec.ArtifactError != nil {
		rec.Status = attemptStatusFailed
	}
	data, err := encodeAttemptRecord(rec, opts.APIKey)
	if err != nil {
		reportAttemptDetail(stderr, err, opts.APIKey)
		return 1
	}
	if err := pub.publishTerminal(data); err != nil {
		reportAttemptDetail(stderr, err, opts.APIKey)
		return 1
	}
	return 0
}

// executeAttempt is the one replay: a recording client, no redirects, no
// client timeout (the attempt context owns the deadline and its cause), and
// a single replayWith call.
func executeAttempt(parent context.Context, opts replayAttemptOptions, trace Trace, target ModelTarget, stderr io.Writer) (replayOutput, []*recordedRequest, *attemptError) {
	recorder := newAttemptRecorder(newAttemptBaseTransport(), opts.AttemptID, maxAttemptResponseBytes)
	client := &http.Client{
		Transport: recorder,
		// A followed redirect would be a second, hidden request (#687).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	transport, err := newCandidateTransport(target, candidateTransportOptions{
		openAICompatBaseURL: opts.BaseURL,
		openAICompatAPIKey:  opts.APIKey,
		httpClient:          client,
	})
	if err != nil {
		reportAttemptDetail(stderr, err, opts.APIKey)
		return replayOutput{}, nil, &attemptError{Code: attemptCodeInput, Stage: attemptStageInput, Message: redactErrorMessage(err.Error())}
	}
	ctx, cancel := context.WithTimeoutCause(parent, opts.Timeout, errAttemptTimeout)
	out, err := replayWith(ctx, transport.chat, target.Model, trace, replayOptions{})
	// Captured before cancel(): afterwards an unexpired context reports
	// context.Canceled, which would read as a signal.
	cause := context.Cause(ctx)
	cancel()
	if err != nil {
		reportAttemptDetail(stderr, err, opts.APIKey)
	}
	rows := recorder.Rows()
	return out, rows, classifyAttempt(err, rows, cause)
}

// loadAttemptInputs validates every input before any request, filling the
// record's identity and settings as far as they are known.
func loadAttemptInputs(opts replayAttemptOptions, rec *attemptRecord) (Trace, ModelTarget, error) {
	if !attemptIDPattern.MatchString(opts.AttemptID) {
		return Trace{}, ModelTarget{}, fmt.Errorf("-attempt-id must match %s", attemptIDPattern)
	}
	if err := validateAttemptArm(opts.Arm); err != nil {
		return Trace{}, ModelTarget{}, err
	}
	if opts.Timeout <= 0 {
		return Trace{}, ModelTarget{}, errors.New("-timeout must be positive")
	}
	targets, err := parseModelTargets(opts.Models)
	if err != nil {
		return Trace{}, ModelTarget{}, fmt.Errorf("-models: %w", err)
	}
	if len(targets) != 1 {
		return Trace{}, ModelTarget{}, fmt.Errorf("-models must name exactly one target, got %d", len(targets))
	}
	target := targets[0]
	if target.Provider != openAICompatTransport {
		return Trace{}, ModelTarget{}, fmt.Errorf("-replay-attempt supports only %s targets, got %q", openAICompatTransport, target.Display)
	}
	rec.Settings.Model = target.Display
	endpoint, err := sanitizeCaptureEndpoint(opts.BaseURL)
	if err != nil || endpoint == "" {
		return Trace{}, ModelTarget{}, errors.New("-candidate-base-url must be an absolute http or https URL")
	}
	// sanitizeCaptureEndpoint accepts any scheme; anything but http(s) would
	// reach the transport and fail there as a connection error.
	if u, _ := url.Parse(endpoint); u.Scheme != "http" && u.Scheme != "https" {
		return Trace{}, ModelTarget{}, errors.New("-candidate-base-url must be an absolute http or https URL")
	}
	rec.Settings.Endpoint = &endpoint
	data, err := os.ReadFile(opts.TracePath)
	if err != nil {
		return Trace{}, ModelTarget{}, fmt.Errorf("read -attempt-trace: %w", err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	rec.Trace.SHA256 = &digest
	var trace Trace
	if err := json.Unmarshal(data, &trace); err != nil {
		return Trace{}, ModelTarget{}, fmt.Errorf("parse -attempt-trace: %w", err)
	}
	// An empty id is unknown, not a value: it stays null (validateTrace then
	// refuses the trace). A real id is published even when validateTrace
	// rejects the trace for another reason.
	if trace.ID != "" {
		id := trace.ID
		rec.Trace.ID = &id
	}
	if err := validateTrace(trace); err != nil {
		return Trace{}, ModelTarget{}, fmt.Errorf("trace %q: %w", trace.ID, err)
	}
	kind := "scripted"
	if prefilledAssemblyMode(trace) {
		kind = "prefilled"
		temp := assemblyCaptureTemperature
		rec.Settings.Temperature = &temp
	}
	rec.ReplayKind = &kind
	return trace, target, nil
}

func validateAttemptArm(arm string) error {
	if arm == "" || len(arm) > 256 || !utf8.ValidString(arm) || strings.ContainsFunc(arm, unicode.IsControl) {
		return errors.New("-attempt-arm must be 1-256 bytes of UTF-8 without control characters")
	}
	return nil
}

func readAttemptBuild() attemptBuild {
	info, _ := debug.ReadBuildInfo() // nil when unavailable
	return attemptBuildFrom(info)
}

// attemptBuildFrom projects build info onto the record; nil means build info
// is unavailable.
func attemptBuildFrom(info *debug.BuildInfo) attemptBuild {
	b := attemptBuild{GoVersion: runtime.Version()}
	if info == nil {
		return b
	}
	b.GoVersion = info.GoVersion
	if v := info.Main.Version; v != "" {
		b.ModuleVersion = &v
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			v := s.Value
			b.VCSRevision = &v
		case "vcs.modified":
			m := s.Value == "true"
			b.VCSModified = &m
		}
	}
	return b
}

func buildAttemptRequestRows(rows []*recordedRequest, refs []attemptRawRefs) []attemptRequestRow {
	out := make([]attemptRequestRow, len(rows))
	for i, row := range rows {
		r := attemptRequestRow{
			Ordinal:     row.Ordinal,
			Correlation: row.Correlation,
			StartedAt:   row.StartedAt.UTC(),
			EndedAt:     row.EndedAt.UTC(),
			// Sub on the original times uses the monotonic clock; UTC() strips it.
			ElapsedMs: row.EndedAt.Sub(row.StartedAt).Milliseconds(),
			Request:   refs[i].Request,
			Response:  refs[i].Response,
			Usage:     row.Usage,
		}
		if !row.HeadersAt.IsZero() {
			h, s := row.HeadersAt.UTC(), row.HTTPStatus
			r.HeadersAt, r.HTTPStatus = &h, &s
		}
		if f := rowFailure(row); f != nil {
			c := f.Code
			r.ErrorCode = &c
		}
		out[i] = r
	}
	return out
}

// encodeAttemptRecord redacts the key in the record's free-text fields, then
// marshals it. Redaction never runs over the encoded bytes, so no key value
// can rewrite structure, identity, hashes or measurements (amendment A3).
func encodeAttemptRecord(rec attemptRecord, apiKey string) ([]byte, error) {
	redactRecordKey(&rec, apiKey)
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode attempt.json: %w", err)
	}
	return append(data, '\n'), nil
}

// redactRecordKey replaces a redactable key in the transcript and notes, the
// only fields that carry candidate, server or trace text.
func redactRecordKey(rec *attemptRecord, key string) {
	if !redactableKey(key) {
		return
	}
	r := func(s string) string { return strings.ReplaceAll(s, key, attemptRedacted) }
	for i := range rec.Transcript {
		t := &rec.Transcript[i]
		t.Content, t.Thinking, t.Name, t.ToolCallID = r(t.Content), r(t.Thinking), r(t.Name), r(t.ToolCallID)
		// Raw is the trace's own tool-turn JSON, copied in verbatim. It is
		// rewritten only when it holds the key: re-encoding sorts object keys
		// and would reformat fixture JSON that never held one.
		if red, held := redactJSONStrings(t.Raw, key); held {
			t.Raw = red
		}
		for j := range t.ToolCalls {
			c := &t.ToolCalls[j]
			c.ID, c.Name = r(c.ID), r(c.Name)
			c.Arguments, _ = redactJSONStrings(c.Arguments, key)
		}
	}
	for i := range rec.Notes {
		rec.Notes[i] = r(rec.Notes[i])
	}
}

// redactJSONStrings rewrites string values and object keys inside raw JSON,
// leaving numbers and structure intact, and reports whether any held the key.
// Transcript arguments come from json.Marshal of a map, so decoding and
// re-encoding them (numbers kept as json.Number) reproduces their bytes apart
// from the redaction. A caller that must keep key-free JSON byte-for-byte
// ignores the result when held is false.
func redactJSONStrings(raw json.RawMessage, key string) (out json.RawMessage, held bool) {
	if len(raw) == 0 {
		return raw, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	// unreachable: transcript arguments are json.Marshal output of a map
	// (assistantTurnFromMessage) and a trace's raw is valid JSON (the trace
	// was parsed), so both decode and re-encode. Replacing what would not
	// with "[REDACTED]", rather than passing unredacted bytes through, is
	// deliberate: it fails closed, and writes nothing to stderr.
	if err := dec.Decode(&v); err != nil {
		return json.RawMessage(strconv.Quote(attemptRedacted)), true
	}
	v, held = redactAttemptJSONValue(v, key)
	out, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(strconv.Quote(attemptRedacted)), true
	}
	return out, held
}

func redactAttemptJSONValue(v any, key string) (any, bool) {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, key, attemptRedacted), strings.Contains(x, key)
	case []any:
		held := false
		for i := range x {
			var h bool
			x[i], h = redactAttemptJSONValue(x[i], key)
			held = held || h
		}
		return x, held
	case map[string]any:
		out := make(map[string]any, len(x))
		held := false
		for k, val := range x {
			nv, h := redactAttemptJSONValue(val, key)
			out[strings.ReplaceAll(k, key, attemptRedacted)] = nv
			held = held || h || strings.Contains(k, key)
		}
		return out, held
	}
	return v, false
}

// reportAttemptDetail writes the full error, key redacted, to stderr: that
// is supervisor evidence, while attempt.json carries only the category
// stub (spec amendment A4).
func reportAttemptDetail(stderr io.Writer, err error, apiKey string) {
	msg := err.Error()
	if apiKey != "" {
		msg = strings.ReplaceAll(msg, apiKey, attemptRedacted)
	}
	_, _ = fmt.Fprintf(stderr, "llm-bench: replay-attempt: %s\n", msg)
}
