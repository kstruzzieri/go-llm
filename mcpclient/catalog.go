package mcpclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/kstruzzieri/go-llm/signing"
)

const catalogFormatVersion = 1

type catalogEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type toolCatalog struct {
	canonical []byte
	entries   []catalogEntry
	pin       string
}

func newToolCatalog(entries []catalogEntry) (toolCatalog, error) {
	owned := make([]catalogEntry, len(entries))
	for i, entry := range entries {
		schema, err := normalizeSchema(entry.InputSchema)
		if err != nil {
			return toolCatalog{}, err
		}
		owned[i] = catalogEntry{
			Name:        entry.Name,
			Description: entry.Description,
			InputSchema: schema,
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })
	canonical, err := signing.MarshalCanonical(owned)
	if err != nil {
		return toolCatalog{}, err
	}
	sum := sha256.Sum256(canonical)
	return toolCatalog{
		canonical: canonical,
		entries:   owned,
		pin:       "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}

func (c toolCatalog) canonicalBytes() []byte {
	return append([]byte(nil), c.canonical...)
}

func (c toolCatalog) digest() string { return c.pin }

func (c toolCatalog) entriesCopy() []catalogEntry {
	entries := make([]catalogEntry, len(c.entries))
	for i, entry := range c.entries {
		entries[i] = entry
		entries[i].InputSchema = append(json.RawMessage(nil), entry.InputSchema...)
	}
	return entries
}

func (toolCatalog) version() int { return catalogFormatVersion }
