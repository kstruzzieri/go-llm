package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

const (
	maxChatRequestBodyBytes       int64 = 8 << 20
	maxCompletionRequestBodyBytes int64 = 4 << 20
	maxEmbeddingRequestBodyBytes  int64 = 4 << 20
)

// decodeJSONBody caps the inbound body before decoding so oversized POSTs fail
// with a bounded client error instead of consuming unbounded memory/CPU ahead
// of the handler's semaphore gate.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("request body exceeds maximum %d bytes", maxErr.Limit))
			return false
		}
		writeError(w, http.StatusBadRequest, "decode_error", err.Error())
		return false
	}
	return true
}
