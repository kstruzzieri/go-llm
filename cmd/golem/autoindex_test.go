package main

import (
	"errors"
	"testing"
)

func TestAutoIndexMode(t *testing.T) {
	tests := []struct {
		name        string
		f           flags
		autoErr     error
		embChainErr error
		want        bool
	}{
		{name: "default on", f: flags{}, want: true},
		{name: "no-rag off", f: flags{noRag: true}, want: false},
		{name: "explicit rag-db off", f: flags{ragDB: "/tmp/x.db"}, want: false},
		{name: "no-auto-index off", f: flags{noAutoIndex: true}, want: false},
		{name: "one-shot off", f: flags{promptSet: true}, want: false},
		{name: "auto path unresolvable off", f: flags{}, autoErr: errors.New("no data dir"), want: false},
		{name: "no embedding chain off", f: flags{}, embChainErr: errors.New("no embedding model configured"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := autoIndexMode(tt.f, tt.autoErr, tt.embChainErr); got != tt.want {
				t.Fatalf("autoIndexMode = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAutoStartLine(t *testing.T) {
	if got := autoStartLine(false); got != "retrieve: building workspace index in the background (first build)" {
		t.Fatalf("absent: %q", got)
	}
	if got := autoStartLine(true); got != "retrieve: refreshing workspace index in the background" {
		t.Fatalf("present: %q", got)
	}
}
