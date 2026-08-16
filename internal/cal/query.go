package cal

import (
	"context"
	"fmt"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
)

// queryEvents fetches every VEVENT (raw — recurring events still carry
// their RRULE, not yet expanded into occurrences) whose recurrence set
// overlaps [timeMin, timeMax] on calendarID.
func queryEvents(ctx context.Context, client *caldav.Client, calendarID string, timeMin, timeMax time.Time) ([]ical.Event, error) {
	query := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:     "VCALENDAR",
			AllProps: true,
			AllComps: true,
		},
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{
				{
					Name:  "VEVENT",
					Start: timeMin,
					End:   timeMax,
				},
			},
		},
	}

	objs, err := client.QueryCalendar(ctx, eventsPath(calendarID), query)
	if err != nil {
		return nil, fmt.Errorf(
			"querying calendar %q: %w (calendar ids other than \"primary\" must be the exact "+
				"email address or id Google Calendar uses for that calendar)", calendarID, err)
	}

	var events []ical.Event
	for _, obj := range objs {
		if obj.Data == nil {
			continue
		}
		events = append(events, obj.Data.Events()...)
	}
	return events, nil
}
