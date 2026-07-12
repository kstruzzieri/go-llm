package rag

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
)

type embeddingFormat string

const (
	embeddingFormatPackedFloat32 embeddingFormat = EmbeddingFormatPackedFloat32
	embeddingFormatLegacyJSON    embeddingFormat = EmbeddingFormatLegacyJSON

	packedEmbeddingMagic                        = "GLLV"
	packedEmbeddingVersion               byte   = 1
	packedEmbeddingElementFloat32        byte   = 1
	packedEmbeddingByteOrderLittleEndian byte   = 1
	packedEmbeddingHeaderSize                   = 16
	maxEmbeddingDimension                uint32 = 1 << 20
)

type corpusEmbeddingDecoder struct {
	format    embeddingFormat
	dimension int
}

func (d *corpusEmbeddingDecoder) decode(encoded []byte, chunkID string) ([]float64, error) {
	vector, format, err := decodeEmbedding(encoded)
	if err != nil {
		return nil, fmt.Errorf("rag: decode embedding for chunk %q: %w", chunkID, err)
	}
	if d.format == "" {
		d.format = format
		d.dimension = len(vector)
		return vector, nil
	}
	if format != d.format {
		return nil, fmt.Errorf("rag: mixed embedding formats: chunk %q uses %s after %s rows", chunkID, format, d.format)
	}
	if len(vector) != d.dimension {
		return nil, fmt.Errorf("rag: embedding dimension mismatch for chunk %q (expected=%d stored=%d)", chunkID, d.dimension, len(vector))
	}
	return vector, nil
}

func encodeEmbedding(vector []float64) ([]byte, error) {
	if len(vector) == 0 || uint64(len(vector)) > uint64(maxEmbeddingDimension) {
		return nil, fmt.Errorf("rag: encode embedding: dimension %d outside 1..%d", len(vector), maxEmbeddingDimension)
	}
	payloadBytes := 4 * len(vector)
	encoded := make([]byte, packedEmbeddingHeaderSize+payloadBytes)
	copy(encoded[:4], packedEmbeddingMagic)
	encoded[4] = packedEmbeddingVersion
	encoded[5] = packedEmbeddingElementFloat32
	encoded[6] = packedEmbeddingByteOrderLittleEndian
	binary.LittleEndian.PutUint32(encoded[8:12], uint32(len(vector)))
	binary.LittleEndian.PutUint32(encoded[12:16], uint32(payloadBytes))
	for i, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("rag: encode embedding: component %d is not finite", i)
		}
		converted := float32(value)
		if math.IsInf(float64(converted), 0) {
			return nil, fmt.Errorf("rag: encode embedding: component %d overflows float32", i)
		}
		binary.LittleEndian.PutUint32(encoded[packedEmbeddingHeaderSize+4*i:], math.Float32bits(converted))
	}
	return encoded, nil
}

func inspectEmbedding(encoded []byte) (embeddingFormat, int, error) {
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		vector, format, err := decodeEmbedding(encoded)
		return format, len(vector), err
	}
	dimension, err := packedEmbeddingDimension(encoded)
	if err != nil {
		return "", 0, err
	}
	for i := range dimension {
		value := math.Float32frombits(binary.LittleEndian.Uint32(encoded[packedEmbeddingHeaderSize+4*i:]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return "", 0, fmt.Errorf("rag: decode packed embedding: component %d is not finite", i)
		}
	}
	return embeddingFormatPackedFloat32, dimension, nil
}

func decodeEmbedding(encoded []byte) ([]float64, embeddingFormat, error) {
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var vector []float64
		if err := json.Unmarshal(trimmed, &vector); err != nil {
			return nil, "", fmt.Errorf("rag: decode legacy JSON embedding: %w", err)
		}
		if err := validateDecodedEmbedding(vector); err != nil {
			return nil, "", err
		}
		return vector, embeddingFormatLegacyJSON, nil
	}
	dimension, err := packedEmbeddingDimension(encoded)
	if err != nil {
		return nil, "", err
	}
	vector := make([]float64, dimension)
	for i := range vector {
		value := math.Float32frombits(binary.LittleEndian.Uint32(encoded[packedEmbeddingHeaderSize+4*i:]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, "", fmt.Errorf("rag: decode packed embedding: component %d is not finite", i)
		}
		vector[i] = float64(value)
	}
	return vector, embeddingFormatPackedFloat32, nil
}

func packedEmbeddingDimension(encoded []byte) (int, error) {
	if len(encoded) < packedEmbeddingHeaderSize {
		return 0, fmt.Errorf("rag: decode packed embedding: truncated header (%d bytes, want %d)", len(encoded), packedEmbeddingHeaderSize)
	}
	if string(encoded[:4]) != packedEmbeddingMagic {
		return 0, fmt.Errorf("rag: decode embedding: unsupported encoding magic %x", encoded[:4])
	}
	if encoded[4] != packedEmbeddingVersion {
		return 0, fmt.Errorf("rag: decode packed embedding: unsupported version %d", encoded[4])
	}
	if encoded[5] != packedEmbeddingElementFloat32 {
		return 0, fmt.Errorf("rag: decode packed embedding: unsupported element type %d", encoded[5])
	}
	if encoded[6] != packedEmbeddingByteOrderLittleEndian {
		return 0, fmt.Errorf("rag: decode packed embedding: unsupported byte order %d", encoded[6])
	}
	if encoded[7] != 0 {
		return 0, fmt.Errorf("rag: decode packed embedding: reserved byte is %d, want 0", encoded[7])
	}
	dimension := binary.LittleEndian.Uint32(encoded[8:12])
	if dimension == 0 || dimension > maxEmbeddingDimension {
		return 0, fmt.Errorf("rag: decode packed embedding: dimension %d outside 1..%d", dimension, maxEmbeddingDimension)
	}
	declaredPayload := binary.LittleEndian.Uint32(encoded[12:16])
	expectedPayload := uint64(dimension) * 4
	if uint64(declaredPayload) != expectedPayload {
		return 0, fmt.Errorf("rag: decode packed embedding: payload length %d does not match dimension %d (%d bytes)", declaredPayload, dimension, expectedPayload)
	}
	if uint64(len(encoded)) != uint64(packedEmbeddingHeaderSize)+expectedPayload {
		return 0, fmt.Errorf("rag: decode packed embedding: payload size is %d bytes, header declares %d", len(encoded)-packedEmbeddingHeaderSize, declaredPayload)
	}
	return int(dimension), nil
}

func validateDecodedEmbedding(vector []float64) error {
	if len(vector) == 0 || uint64(len(vector)) > uint64(maxEmbeddingDimension) {
		return fmt.Errorf("rag: decode embedding: dimension %d outside 1..%d", len(vector), maxEmbeddingDimension)
	}
	for i, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("rag: decode embedding: component %d is not finite", i)
		}
	}
	return nil
}
