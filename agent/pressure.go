package agent

// PressureLevel is the severity of per-turn context-budget usage. It is a label
// only: LevelCritical does not stop a run (the hard stop is ErrContextExhausted).
type PressureLevel int

const (
	LevelOK PressureLevel = iota
	LevelWatch
	LevelWarn
	LevelCritical
)

func (l PressureLevel) String() string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelWatch:
		return "watch"
	case LevelWarn:
		return "warn"
	case LevelCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// PressureCause names the dominant input bucket driving a turn's token usage.
type PressureCause int

const (
	CauseUnknown PressureCause = iota
	CausePinned
	CauseToolSchema
	CauseHistory
	CauseToolOutput
	CauseRetrieval
)

func (c PressureCause) String() string {
	switch c {
	case CausePinned:
		return "pinned"
	case CauseToolSchema:
		return "tool_schema"
	case CauseHistory:
		return "history"
	case CauseToolOutput:
		return "tool_output"
	case CauseRetrieval:
		return "retrieval"
	default:
		return "unknown"
	}
}

// PressureMitigation names what the runtime did (or advises) for a turn.
// MitigationHalt is emitted only on ErrContextExhausted. A summarizing-compaction
// mitigation is intentionally absent until #58 ships it.
type PressureMitigation int

const (
	MitigationNone PressureMitigation = iota
	MitigationWarn
	MitigationEvict
	MitigationHalt
)

func (m PressureMitigation) String() string {
	switch m {
	case MitigationWarn:
		return "warn"
	case MitigationEvict:
		return "evict"
	case MitigationHalt:
		return "halt"
	default:
		return "none"
	}
}

// dominantCause attributes a turn's input tokens to the single largest bucket.
// It reuses ContextManager.estimate / messageCost so the attribution can never
// drift from the token accounting Assemble performs. Per-message precedence:
// pinned > retrieval (Attrib set) > tool_output (role "tool") > history. Ties
// resolve to the earliest bucket in {pinned, tool_schema, history, tool_output,
// retrieval}; an all-zero state yields CauseUnknown.
func (m ContextManager) dominantCause(st State, toolSchemaTokens int) PressureCause {
	var pinned, toolOutput, retrieval, history int
	pinned += m.estimate(st.System)
	for _, msg := range st.Messages {
		cost := m.messageCost(msg)
		switch {
		case msg.Segment == Pinned:
			pinned += cost
		case msg.Attrib != nil:
			retrieval += cost
		case msg.Role == "tool":
			toolOutput += cost
		default:
			history += cost
		}
	}
	buckets := []struct {
		cause  PressureCause
		tokens int
	}{
		{CausePinned, pinned},
		{CauseToolSchema, toolSchemaTokens},
		{CauseHistory, history},
		{CauseToolOutput, toolOutput},
		{CauseRetrieval, retrieval},
	}
	best, bestTokens := CauseUnknown, 0
	for _, b := range buckets {
		if b.tokens > bestTokens {
			best, bestTokens = b.cause, b.tokens
		}
	}
	return best
}

// PressureThresholds are the fractional bands (of the per-turn input budget) that
// classify usage. The zero value normalizes to conservative defaults so existing
// Budget{} callers are unaffected.
type PressureThresholds struct {
	Watch    float64
	Warn     float64
	Critical float64
}

// normalize fills zero fields with their default and, if the result is not a
// valid monotonic triplet (0 < Watch <= Warn <= Critical <= 1), falls back to the
// default triplet entirely. This keeps Classify deterministic for any input.
func (t PressureThresholds) normalize() PressureThresholds {
	def := PressureThresholds{Watch: 0.60, Warn: 0.75, Critical: 0.90}
	if t == (PressureThresholds{}) {
		return def
	}
	if t.Watch == 0 {
		t.Watch = def.Watch
	}
	if t.Warn == 0 {
		t.Warn = def.Warn
	}
	if t.Critical == 0 {
		t.Critical = def.Critical
	}
	if !(0 < t.Watch && t.Watch <= t.Warn && t.Warn <= t.Critical && t.Critical <= 1) {
		return def
	}
	return t
}

// Classify maps a used fraction (plus the exhausted/evicted facts known at
// assembly) to a level and a mitigation. exhausted overrides everything with
// LevelCritical/MitigationHalt. Mitigation precedence: halt > evict > warn > none.
func (t PressureThresholds) Classify(usedPct float64, exhausted, evicted bool) (PressureLevel, PressureMitigation) {
	if exhausted {
		return LevelCritical, MitigationHalt
	}
	level := LevelOK
	switch {
	case usedPct >= t.Critical:
		level = LevelCritical
	case usedPct >= t.Warn:
		level = LevelWarn
	case usedPct >= t.Watch:
		level = LevelWatch
	}
	switch {
	case evicted:
		return level, MitigationEvict
	case level >= LevelWarn:
		return level, MitigationWarn
	default:
		return level, MitigationNone
	}
}
