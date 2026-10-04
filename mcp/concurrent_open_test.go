package mcp

import (
	"context"
	"io"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

func TestOpenRetrievalFeedbackWeighterConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		db, _, err := openRetrievalFeedbackWeighter(ctx, path)
		if err != nil {
			return nil, err
		}
		return db, nil
	})
}
