package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

func TestOpenSessionConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		s, _, err := openSession(ctx, path, "concurrent")
		if err != nil {
			return nil, err
		}
		return s, nil
	})
}

func TestOpenFeedbackServiceConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		svc, err := openFeedbackService(ctx, filepath.Dir(path), path, func(string) {})
		if err != nil {
			return nil, err
		}
		return closeFunc(func() error { _, err := svc.close(); return err }), nil
	})
}
