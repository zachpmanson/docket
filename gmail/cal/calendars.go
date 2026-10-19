package cal

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-webdav/caldav"
)

// Calendar is a calendar collection on the account's calendar home, as
// discovered via calendar-home PROPFIND: ID is the calendar id accepted by
// --calendar/eventsPath (the account's email address for its own primary
// calendar, or Google's opaque id for shared/extra calendars), Name is the
// calendar's display name.
type Calendar struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListCalendars returns calendars that support time-range REPORT queries.
// Virtual calendars are excluded so --calendar all sweeps remain query-safe.
func ListCalendars(ctx context.Context, client *caldav.Client, homeID string) ([]Calendar, error) {
	cals, err := DiscoverCalendars(ctx, client, homeID)
	if err != nil {
		return nil, err
	}
	out := make([]Calendar, 0, len(cals))
	for _, c := range cals {
		if !strings.HasSuffix(c.ID, "@virtual") {
			out = append(out, c)
		}
	}
	return out, nil
}

// DiscoverCalendars discovers every calendar exposed by the account's CalDAV
// home, including virtual/read-only calendars. homeID is the account email.
// Discovery does not imply write permission on a returned calendar.
func DiscoverCalendars(ctx context.Context, client *caldav.Client, homeID string) ([]Calendar, error) {
	home := "/caldav/v2/" + homeID + "/"
	cals, err := client.FindCalendars(ctx, home)
	if err != nil {
		return nil, fmt.Errorf("listing calendars under %q: %w", home, err)
	}

	out := make([]Calendar, 0, len(cals))
	for _, c := range cals {
		// Collection paths are "<home>events/" for the account's own primary
		// calendar and "/caldav/v2/<id>/events/" for every other calendar,
		// so the id is always the third path segment.
		parts := strings.Split(strings.Trim(c.Path, "/"), "/")
		if len(parts) < 4 || parts[0] != "caldav" || parts[1] != "v2" || parts[len(parts)-1] != "events" {
			continue
		}
		id := parts[2]
		out = append(out, Calendar{ID: id, Name: c.Name})
	}
	return out, nil
}

// AgendaAll lists concrete occurrences across every calendar the account
// can see, merged and sorted by start time (recurring expansion, overrides,
// and all-day handling all reuse Agenda/expandEvents unchanged). Each
// event's Calendar/CalendarName name its source so multi-calendar output
// stays attributable.
func AgendaAll(ctx context.Context, client *caldav.Client, homeID string, timeMin, timeMax time.Time, loc *time.Location) ([]Event, error) {
	cals, err := ListCalendars(ctx, client, homeID)
	if err != nil {
		return nil, err
	}

	var all []Event
	for _, c := range cals {
		events, err := Agenda(ctx, client, c.ID, timeMin, timeMax, loc)
		if err != nil {
			return nil, err
		}
		for i := range events {
			events[i].CalendarName = c.Name
			all = append(all, events[i])
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Start < all[j].Start })
	return all, nil
}
