package providerbootstrap

import "testing"

func TestBundleCloseNilSafe(t *testing.T) {
	var b *Bundle
	if err := b.Close(); err != nil {
		t.Fatalf("nil Bundle.Close() = %v, want nil", err)
	}
	if err := (&Bundle{}).Close(); err != nil {
		t.Fatalf("empty Bundle.Close() = %v, want nil", err)
	}
}
