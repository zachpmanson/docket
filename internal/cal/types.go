package cal

// Event is the shape returned by agenda/show. Start/End are always RFC3339
// with an explicit offset — never bare local times — per docket-design.md
// §5. AllDay events carry a date-only Start/End instead.
//
// ID is a synthetic "<UID>::<occurrence-start-RFC3339>" composite, not a
// CalDAV href — recurring events share one href across every occurrence,
// so the href alone can't address a specific instance. Show re-derives the
// occurrence by re-querying a window around the embedded timestamp.
type Event struct {
	ID        string   `json:"id"`
	Summary   string   `json:"summary"`
	Start     string   `json:"start"`
	End       string   `json:"end"`
	AllDay    bool     `json:"all_day"`
	Location  string   `json:"location,omitempty"`
	Attendees []string `json:"attendees,omitempty"`
	Status    string   `json:"status"`
	Calendar  string   `json:"calendar"`

	// Transparent is true when the event is marked TRANSP:TRANSPARENT
	// (Google's "Show me as available during this event"). Not part of the
	// agent-facing shape — used internally by FreeBusy to decide what
	// counts as busy.
	Transparent bool `json:"-"`
}

// TimeRange is a start/end pair, always RFC3339 with an explicit offset.
type TimeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// FreeBusyResult is the shape returned by the freebusy command: busy ranges
// per requested calendar.
type FreeBusyResult struct {
	Busy map[string][]TimeRange `json:"busy"`
}
