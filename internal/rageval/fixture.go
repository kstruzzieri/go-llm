package rageval

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadFixture reads a golden RAG evaluation fixture from disk.
func LoadFixture(path string) (*Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rag eval: read fixture: %w", err)
	}
	var fixture Fixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return nil, fmt.Errorf("rag eval: parse fixture: %w", err)
	}
	if err := fixture.Validate(); err != nil {
		return nil, err
	}
	return &fixture, nil
}

// Validate checks the fixture's internal references.
func (f *Fixture) Validate() error {
	if f == nil {
		return fmt.Errorf("rag eval: fixture is nil")
	}
	if len(f.Corpus) == 0 {
		return fmt.Errorf("rag eval: fixture corpus is empty")
	}
	if len(f.Queries) == 0 {
		return fmt.Errorf("rag eval: fixture queries are empty")
	}

	chunks := make(map[string]FixtureChunk, len(f.Corpus))
	var dim int
	for i, chunk := range f.Corpus {
		if chunk.ID == "" {
			return fmt.Errorf("rag eval: corpus chunk %d missing id", i)
		}
		if _, exists := chunks[chunk.ID]; exists {
			return fmt.Errorf("rag eval: duplicate chunk id %q", chunk.ID)
		}
		if chunk.Source == "" {
			return fmt.Errorf("rag eval: chunk %q missing source", chunk.ID)
		}
		if len(chunk.Embedding) == 0 {
			return fmt.Errorf("rag eval: chunk %q missing embedding", chunk.ID)
		}
		if dim == 0 {
			dim = len(chunk.Embedding)
		}
		if len(chunk.Embedding) != dim {
			return fmt.Errorf("rag eval: chunk %q embedding dim=%d, want %d", chunk.ID, len(chunk.Embedding), dim)
		}
		chunks[chunk.ID] = chunk
	}

	queries := make(map[string]struct{}, len(f.Queries))
	for i, query := range f.Queries {
		if query.ID == "" {
			return fmt.Errorf("rag eval: query %d missing id", i)
		}
		if _, exists := queries[query.ID]; exists {
			return fmt.Errorf("rag eval: duplicate query id %q", query.ID)
		}
		if query.Category == "" {
			return fmt.Errorf("rag eval: query %q missing category", query.ID)
		}
		if query.Query == "" {
			return fmt.Errorf("rag eval: query %q missing query text", query.ID)
		}
		if len(query.Embedding) != dim {
			return fmt.Errorf("rag eval: query %q embedding dim=%d, want %d", query.ID, len(query.Embedding), dim)
		}
		if len(query.ExpectedIDs) == 0 {
			return fmt.Errorf("rag eval: query %q missing expected ids", query.ID)
		}
		for _, id := range query.ExpectedIDs {
			if _, ok := chunks[id]; !ok {
				return fmt.Errorf("rag eval: query %q references unknown chunk %q", query.ID, id)
			}
		}
		queries[query.ID] = struct{}{}
	}
	return nil
}
