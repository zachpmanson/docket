package cal

import (
	"context"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
)

// FreeBusy derives busy time ranges for calendarID between start and end
// from its expanded events. Google's CalDAV interface has no
// free-busy-query REPORT (unlike the REST API), so this is computed
// ourselves: any non-cancelled event not explicitly marked TRANSPARENT
// counts as busy, matching how Google Calendar's own UI treats "Show me
// as available during this event". See docket-design.md §5.
func FreeBusy(ctx context.Context, client *caldav.Client, calendarIDs []string, start, end time.Time, loc *time.Location) (*FreeBusyResult, error) {
	busy := make(map[string][]TimeRange, len(calendarIDs))

	for _, id := range calendarIDs {
		raw, err := queryEvents(ctx, client, id, start, end)
		if err != nil {
			return nil, err
		}
		events, err := expandEvents(raw, start, end, loc, id)
		if err != nil {
			return nil, err
		}

		var ranges []TimeRange
		for i := range events {
			e := &events[i]
			if e.Status == string(ical.EventCancelled) || e.Transparent {
				continue
			}
			ranges = append(ranges, TimeRange{Start: e.Start, End: e.End})
		}
		busy[id] = ranges
	}

	return &FreeBusyResult{Busy: busy}, nil
}
