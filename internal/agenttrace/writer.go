package agenttrace

import (
	"encoding/json"
	"fmt"
	"os"
)

const fileMode os.FileMode = 0o600

// jsonlWriter appends content-light telemetry spans to a caller-supplied path,
// one JSON object per line, each written in a single Write so concurrent
// processes sharing the file do not interleave partial lines. It owns no path
// policy. Used by one run's TelemetrySink, whose callbacks are serial.
type jsonlWriter struct {
	f *os.File
}

func openJSONL(path string) (*jsonlWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		return nil, fmt.Errorf("agenttrace: open telemetry %q: %w", path, err)
	}
	return &jsonlWriter{f: f}, nil
}

func (w *jsonlWriter) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("agenttrace: marshal span: %w", err)
	}
	b = append(b, '\n')
	if _, err := w.f.Write(b); err != nil {
		return fmt.Errorf("agenttrace: write span: %w", err)
	}
	return nil
}

func (w *jsonlWriter) Close() error { return w.f.Close() }

// WriteTrace writes one content-full trace to path, create-exclusive so a
// run-id collision returns an error (the caller retries with a suffixed path)
// instead of appending to or clobbering an existing trace.
func WriteTrace(path string, rec TraceRecord) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("agenttrace: create trace %q: %w", path, err)
	}
	defer func() {
		// A failed Close on a written file can mean unflushed/lost data, so
		// surface it — but never mask an earlier encode error.
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("agenttrace: close trace %q: %w", path, cerr)
		}
	}()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(rec); encErr != nil {
		return fmt.Errorf("agenttrace: encode trace %q: %w", path, encErr)
	}
	return nil
}
