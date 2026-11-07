package conversation

// MaxExposureFirstBytes bounds how much of the first exposing content an
// Exposure keeps in First, so a record never stores a large payload.
const MaxExposureFirstBytes = 128

// Exposure records whether outside content has entered a conversation.
//
// The zero value means no record exists, so it reads as exposed: an
// unrecorded conversation is treated as unknown rather than clean.
type Exposure struct {
	// Recorded is true when an exposure record exists for the conversation.
	Recorded bool
	// Foreign is true when content from outside the conversation has entered it.
	Foreign bool
	// Unknown is true when the origin of entered content could not be determined.
	Unknown bool
	// First holds at most MaxExposureFirstBytes of the first exposing content.
	First string
}

// Exposed reports whether the conversation should be treated as exposed to
// outside content. A missing record, foreign content, or unknown origin all
// count as exposed.
func (e Exposure) Exposed() bool {
	return !e.Recorded || e.Foreign || e.Unknown
}
