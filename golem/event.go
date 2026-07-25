package golem

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

const (
	maxDisplayBytes = 8 * 1024
	maxEventBytes   = 128 * 1024
	truncatedMarker = "…[truncated]"
)

var errEventTooLarge = errors.New("golem: event exceeds 128 KiB")

func truncatePreview(value string) string {
	return truncateDisplay(value)
}

func truncateErrorMessage(value string) string {
	return truncateDisplay(value)
}

func truncateDisplay(value string) string {
	if len(value) <= maxDisplayBytes {
		return value
	}
	end := maxDisplayBytes - len(truncatedMarker)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + truncatedMarker
}

func splitDelta(text string, fits func(string) (bool, error)) ([]string, error) {
	if text == "" {
		return nil, nil
	}

	boundaries := make([]int, 1, utf8.RuneCountInString(text)+1)
	for i := range text {
		if i != 0 {
			boundaries = append(boundaries, i)
		}
	}
	boundaries = append(boundaries, len(text))

	var chunks []string
	for start := 0; start < len(boundaries)-1; {
		low, high, best := start+1, len(boundaries)-1, -1
		for low <= high {
			mid := low + (high-low)/2
			ok, err := fits(text[boundaries[start]:boundaries[mid]])
			if err != nil {
				return nil, fmt.Errorf("golem: size delta event: %w", err)
			}
			if ok {
				best = mid
				low = mid + 1
			} else {
				high = mid - 1
			}
		}
		if best < 0 {
			return nil, errEventTooLarge
		}
		chunks = append(chunks, text[boundaries[start]:boundaries[best]])
		start = best
	}
	return chunks, nil
}

func validateEventSize(event Event) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("golem: marshal event: %w", err)
	}
	if len(raw) > maxEventBytes {
		return fmt.Errorf("%w: got %d bytes", errEventTooLarge, len(raw))
	}
	return nil
}
