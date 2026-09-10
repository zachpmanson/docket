package cal

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// expandEvents turns raw VEVENTs (as returned by queryEvents, recurring
// ones still carrying an unexpanded RRULE) into concrete occurrences
// within [timeMin, timeMax], mirroring what the REST API's
// singleEvents=true does server-side. See docket-design.md §5.
//
// Known limitation: a modified occurrence (RECURRENCE-ID override) is only
// found if its *original* scheduled time falls within the query window —
// an instance moved from just outside the window to inside it (or vice
// versa) won't be matched against its override. This is an edge case
// judged acceptable for phase 2.
func expandEvents(events []ical.Event, timeMin, timeMax time.Time, loc *time.Location, calendarID string) ([]Event, error) {
	masters := make(map[string]*ical.Event)
	overrides := make(map[string]map[time.Time]*ical.Event)

	for i := range events {
		e := &events[i]
		uid, _ := e.Props.Text(ical.PropUID)

		if recurIDProp := e.Props.Get(ical.PropRecurrenceID); recurIDProp != nil {
			t, err := recurIDProp.DateTime(loc)
			if err != nil {
				return nil, fmt.Errorf("parsing RECURRENCE-ID for event %q: %w", uid, err)
			}
			if overrides[uid] == nil {
				overrides[uid] = make(map[time.Time]*ical.Event)
			}
			overrides[uid][t.UTC()] = e
		} else {
			masters[uid] = e
		}
	}

	var out []Event

	for uid, master := range masters {
		set, err := master.RecurrenceSet(loc)
		if err != nil {
			return nil, fmt.Errorf("expanding recurrence for event %q: %w", uid, err)
		}

		start, err := master.DateTimeStart(loc)
		if err != nil {
			return nil, fmt.Errorf("parsing start time for event %q: %w", uid, err)
		}
		end, err := master.DateTimeEnd(loc)
		if err != nil {
			return nil, fmt.Errorf("parsing end time for event %q: %w", uid, err)
		}
		allDay := isAllDay(master)

		if set == nil {
			if end.After(timeMin) && start.Before(timeMax) {
				out = append(out, eventFromICal(master, uid, start, allDay, calendarID))
			}
			continue
		}

		ovs := overrides[uid]
		for _, occStart := range set.Between(timeMin, timeMax, true) {
			if ov, ok := ovs[occStart.UTC()]; ok {
				if status, _ := ov.Status(); status == ical.EventCancelled {
					continue
				}
				ovStart, err := ov.DateTimeStart(loc)
				if err != nil {
					return nil, fmt.Errorf("parsing overridden start time for event %q: %w", uid, err)
				}
				out = append(out, eventFromICal(ov, uid, ovStart, isAllDay(ov), calendarID))
				continue
			}
			out = append(out, eventFromICal(master, uid, occStart, allDay, calendarID))
		}
	}

	// An override whose master's own scheduled time falls outside the query
	// window still needs to appear if the override itself falls inside it.
	for uid, ovs := range overrides {
		if _, hasMaster := masters[uid]; hasMaster {
			continue
		}
		for _, ov := range ovs {
			if status, _ := ov.Status(); status == ical.EventCancelled {
				continue
			}
			start, err := ov.DateTimeStart(loc)
			if err != nil {
				continue
			}
			end, err := ov.DateTimeEnd(loc)
			if err != nil {
				continue
			}
			if end.After(timeMin) && start.Before(timeMax) {
				out = append(out, eventFromICal(ov, uid, start, isAllDay(ov), calendarID))
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}

func isAllDay(e *ical.Event) bool {
	prop := e.Props.Get(ical.PropDateTimeStart)
	return prop != nil && prop.ValueType() == ical.ValueDate
}

func eventFromICal(e *ical.Event, uid string, occStart time.Time, allDay bool, calendarID string) Event {
	summary, _ := e.Props.Text(ical.PropSummary)
	location, _ := e.Props.Text(ical.PropLocation)
	status, _ := e.Status()
	transp, _ := e.Props.Text(ical.PropTransparency)

	loc := occStart.Location()
	start, _ := e.DateTimeStart(loc)
	end, _ := e.DateTimeEnd(loc)
	duration := end.Sub(start)
	occEnd := occStart.Add(duration)

	var attendees []string
	for _, prop := range e.Props.Values(ical.PropAttendee) {
		attendees = append(attendees, strings.TrimPrefix(strings.ToLower(prop.Value), "mailto:"))
	}

	formatTime := func(t time.Time) string {
		if allDay {
			return t.Format("2006-01-02")
		}
		return t.Format(time.RFC3339)
	}

	return Event{
		ID:          uid + "::" + occStart.Format(time.RFC3339),
		Summary:     summary,
		Start:       formatTime(occStart),
		End:         formatTime(occEnd),
		AllDay:      allDay,
		Location:    location,
		Attendees:   attendees,
		Status:      string(status),
		Calendar:    calendarID,
		Transparent: strings.EqualFold(transp, "TRANSPARENT"),
	}
}
