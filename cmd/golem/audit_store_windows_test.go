package main

import "testing"

func TestAuditStoreWindowsURI(t *testing.T) {
	got, err := auditStoreURI(`C:\audit files\100%.db`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "file:///C:/audit%20files/100%25.db?cache=private&immutable=1&mode=ro"; got != want {
		t.Fatalf("URI = %q; want %q", got, want)
	}
}
