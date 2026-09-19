package secretscan_test

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/secretscan"
	"github.com/kstruzzieri/go-llm/rag"
)

// Ordered raw snapshot from b97613be5eca70205fb92b0cf114236f36e37fd4.
//
//go:embed testdata/clean-corpus.fixture
var cleanCorpus string

func TestPinnedCleanCorpus(t *testing.T) {
	t.Parallel()
	manifest := []struct {
		path string
		size int
		hash string
	}{
		{"testdata/sample.go", 291, "e78c9ba1d9ae1e32c3fb39b71a90b7973bca494a71afd26e2936b90eda512cd8"},
		{"testdata/sample.py", 673, "331652770a2a1f8d4b6b1f50cde70f203119d4cd4c70d1cb0f7584f6b5b98db4"},
		{"testdata/sample.ts", 628, "8f6e63dd8421790f9097851b9d3020d6bf465dcb5b562e47263128c923cddcfb"},
		{"testdata/plain.txt", 722, "f321a3488843fff6535846fafed0c670757f990f1f754bb9df80bd45cdc1f05a"},
		{"README.md", 42592, "18a14a047b488117bb23dbef99e2664ae06ab68fc2ab316b286b09268078a131"},
		{"docs/DESIGN.md", 21535, "f2600d8b35c40d6cb4a9b5b5126f61b2eb1b25a6a14e0ca4450a321fcdae2f4d"},
		{"agent/interceptor/policy.go", 5863, "0768fe60787264b48e99c1de6d1d78349e2388333bb5b323614b7fabca9c1a20"},
		{"rag/indexer.go", 23841, "c525476573cf1711dd470bb0135c625a2c661292a7a11877bcb147100c4e7b9c"},
		{"cmd/llm-bench/capture.go", 24249, "ca6f97544cd4aa134a12ae68b52000e3bf3696c19422f27e7de29da49a7ccd68"},
		{"signing/keyfile.go", 14614, "db55296efcc849b914ab991adc9184bd16246049cd3079dac867294d3e481fd6"},
		{"go.sum", 8967, "b07e266281f312d9a74c3ef27d8e972adafdd812be4814bb2ca6364788da6e36"},
	}
	if len(cleanCorpus) != 143975 {
		t.Fatalf("fixture bytes = %d, want 143975", len(cleanCorpus))
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(cleanCorpus))); got != "6f75107836d875377fd035e5f7094f73d4dac1999c1742536a76cf77f76a5d3d" {
		t.Fatalf("fixture aggregate SHA-256 = %s, want pinned checksum", got)
	}
	start := 0
	for _, file := range manifest {
		content := cleanCorpus[start : start+file.size]
		start += file.size
		t.Run(file.path, func(t *testing.T) {
			t.Parallel()
			if got := fmt.Sprintf("%x", sha256.Sum256([]byte(content))); got != file.hash {
				t.Fatalf("file SHA-256 = %s, want %s", got, file.hash)
			}
			if got := secretscan.Scan(content); len(got) != 0 {
				t.Errorf("whole-file Scan = %v, want no findings", got)
			}
			chunks, err := rag.NewCodeChunker().Chunk(file.path, content)
			if err != nil {
				t.Fatalf("chunk pinned file: %v", err)
			}
			for i, chunk := range chunks {
				if got := secretscan.Scan(chunk.Content); len(got) != 0 {
					t.Errorf("chunk %d Scan = %v, want no findings", i, got)
				}
			}
		})
	}
	if start != len(cleanCorpus) {
		t.Errorf("manifest total = %d, want %d", start, len(cleanCorpus))
	}
}
