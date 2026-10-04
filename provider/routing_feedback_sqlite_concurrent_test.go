package provider

import (
	"context"
	"io"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

func TestOpenSQLiteFeedbackStoreConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		s, err := OpenSQLiteFeedbackStore(ctx, path, SQLiteFeedbackStoreConfig{})
		if err != nil {
			return nil, err
		}
		return s, nil
	})
}
