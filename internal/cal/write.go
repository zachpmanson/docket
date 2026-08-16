package cal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
	"github.com/teambition/rrule-go"
)

// ParseRRule validates a raw RFC 5545 RRULE value (e.g.
// "FREQ=WEEKLY;BYDAY=MO,WE,FR"), the same native-syntax-passthrough
// approach used elsewhere in docket rather than a bespoke recurrence DSL.
func ParseRRule(rule string) (*rrule.ROption, error) {
	opt, err := rrule.StrToROption(rule)
	if err != nil {
		return nil, fmt.Errorf(
			"could not parse --rrule %q as an RFC 5545 recurrence rule: %w "+
				"(e.g. \"FREQ=WEEKLY;BYDAY=MO,WE,FR\", \"FREQ=DAILY;COUNT=10\")", rule, err)
	}
	return opt, nil
}

// isNotFoundErr detects a CalDAV 404 by matching the error text — the
// go-webdav library's HTTPError type lives in an unexported internal
// package, so there's no typed way to check the status code from outside
// the module. Fragile but the only option available.
func isNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "404")
}

func eventObjectPath(calendarID, uid string) string {
	return eventsPath(calendarID) + "/" + uid + ".ics"
}

// eventUID returns a deterministic uid derived from idempotencyKey (so a
// retried create with the same key lands on the same CalDAV resource
// instead of creating a duplicate — see docket-design.md §5), or a random
// one if no key was given.
func eventUID(idempotencyKey string) (string, error) {
	if idempotencyKey != "" {
		sum := sha256.Sum256([]byte(idempotencyKey))
		return "docket-" + hex.EncodeToString(sum[:16]) + "@docket", nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating event id: %w", err)
	}
	return hex.EncodeToString(buf) + "@docket", nil
}

func buildEventCalendar(uid, summary string, start, end time.Time, allDay bool, location string, attendees []string, rr *rrule.ROption) *ical.Calendar {
	root := ical.NewCalendar()
	root.Props.SetText(ical.PropProductID, "-//docket//EN")
	root.Props.SetText(ical.PropVersion, "2.0")

	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, uid)
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	if allDay {
		event.Props.SetDate(ical.PropDateTimeStart, start)
		event.Props.SetDate(ical.PropDateTimeEnd, end)
	} else {
		// TZID-qualified, not UTC: confirmed live that Google's CalDAV time-
		// range REPORT (what Agenda/FreeBusy/find-slot use) silently excludes
		// events written with a UTC "Z"-suffixed DTSTART/DTEND, even though
		// that's valid per RFC 5545 — direct GET-by-path still returns them
		// fine, only the time-range index misses them. TZID matching
		// Google's own event convention (e.g. "Australia/Melbourne") is
		// indexed correctly and needs no accompanying VTIMEZONE component;
		// Google resolves the zone by name server-side rather than requiring
		// one embedded in the submitted calendar (native Google-created
		// events don't carry one either).
		event.Props.SetDateTime(ical.PropDateTimeStart, start)
		event.Props.SetDateTime(ical.PropDateTimeEnd, end)
	}
	event.Props.SetText(ical.PropSummary, summary)
	if location != "" {
		event.Props.SetText(ical.PropLocation, location)
	}
	for _, a := range attendees {
		prop := ical.NewProp(ical.PropAttendee)
		prop.Value = "mailto:" + a
		event.Props.Add(prop)
	}
	event.Props.SetText(ical.PropStatus, "CONFIRMED")
	if rr != nil {
		event.Props.SetRecurrenceRule(rr)
	}

	root.Children = append(root.Children, event.Component)
	return root
}

// findMasterComponent locates the non-override VEVENT for uid within a
// fetched calendar object, mirroring the master/override distinction
// expand.go makes when reading events back.
func findMasterComponent(cal *ical.Calendar, uid string) (*ical.Component, error) {
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		childUID, _ := child.Props.Text(ical.PropUID)
		if childUID != uid {
			continue
		}
		if child.Props.Get(ical.PropRecurrenceID) != nil {
			continue
		}
		return child, nil
	}
	return nil, fmt.Errorf(
		"no event found with uid %q on this calendar — it may have been deleted, or this id "+
			"refers only to a modified occurrence of a series whose master couldn't be found", uid)
}

// CreatePlan is a fully-resolved, ready-to-create event. Building one never
// mutates anything — safe to preview before Execute.
type CreatePlan struct {
	Summary   string   `json:"summary"`
	Start     string   `json:"start"`
	End       string   `json:"end"`
	AllDay    bool     `json:"all_day"`
	Location  string   `json:"location,omitempty"`
	Attendees []string `json:"attendees,omitempty"`
	RRule     string   `json:"rrule,omitempty"`
	Calendar  string   `json:"calendar"`
	UID       string   `json:"uid"`

	calendarID string
	startTime  time.Time
	ical       *ical.Calendar
	path       string
	idempotent bool
}

// PrepareCreate validates and builds a new event, without creating
// anything. idempotencyKey, if non-empty, makes the resulting uid (and
// hence Execute) idempotent — see docket-design.md §5. rr, if non-nil,
// makes the event recurring (see ParseRRule).
func PrepareCreate(calendarID, summary string, start, end time.Time, allDay bool, location string, attendees []string, rr *rrule.ROption, rruleText, idempotencyKey string) (*CreatePlan, error) {
	if !end.After(start) {
		return nil, fmt.Errorf("event end (%s) must be after its start (%s)",
			end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	uid, err := eventUID(idempotencyKey)
	if err != nil {
		return nil, err
	}

	formatTime := func(t time.Time) string {
		if allDay {
			return t.Format("2006-01-02")
		}
		return t.Format(time.RFC3339)
	}

	return &CreatePlan{
		Summary: summary, Start: formatTime(start), End: formatTime(end), AllDay: allDay,
		Location: location, Attendees: attendees, RRule: rruleText, Calendar: calendarID, UID: uid,
		calendarID: calendarID, startTime: start,
		ical:       buildEventCalendar(uid, summary, start, end, allDay, location, attendees, rr),
		path:       eventObjectPath(calendarID, uid),
		idempotent: idempotencyKey != "",
	}, nil
}

// Execute creates the prepared event. If idempotencyKey was set and an
// event with the derived uid already exists, that existing event is
// returned rather than creating a duplicate.
func (p *CreatePlan) Execute(ctx context.Context, client *caldav.Client) (*Event, error) {
	if p.idempotent {
		if existing, err := client.GetCalendarObject(ctx, p.path); err == nil {
			if events := existing.Data.Events(); len(events) > 0 {
				start, _ := events[0].DateTimeStart(time.UTC)
				ev := eventFromICal(&events[0], p.UID, start, isAllDay(&events[0]), p.calendarID)
				return &ev, nil
			}
		} else if !isNotFoundErr(err) {
			return nil, fmt.Errorf("checking for existing event %q: %w", p.UID, err)
		}
	}

	if _, err := client.PutCalendarObject(ctx, p.path, p.ical); err != nil {
		return nil, fmt.Errorf("creating event on calendar %q: %w", p.calendarID, err)
	}

	master := p.ical.Events()[0]
	ev := eventFromICal(&master, p.UID, p.startTime, p.AllDay, p.calendarID)
	return &ev, nil
}

// UpdatePlan is a fully-resolved event update. Building one fetches the
// existing event (a read-only call) but doesn't write anything.
type UpdatePlan struct {
	ID       string            `json:"id"`
	Calendar string            `json:"calendar"`
	Changes  map[string]string `json:"changes"`
	Warning  string            `json:"warning,omitempty"`

	calendarID string
	uid        string
	path       string
	calObj     *ical.Calendar
}

// PrepareUpdate fetches the event named by id and applies whichever of
// newSummary/newStart+newEnd/newLocation are non-nil, without writing
// anything. Updating a recurring event's id affects the entire series, not
// just that occurrence — see docket-design.md §5's known limitations;
// Warning is set when this applies.
func PrepareUpdate(ctx context.Context, client *caldav.Client, calendarID, id string, loc *time.Location,
	newSummary *string, newStart, newEnd *time.Time, allDay bool, newLocation *string) (*UpdatePlan, error) {

	uid, _, err := parseEventID(id, loc)
	if err != nil {
		return nil, err
	}
	path := eventObjectPath(calendarID, uid)

	obj, err := client.GetCalendarObject(ctx, path)
	if err != nil {
		return nil, fmt.Errorf(
			"fetching event %q to update: %w (event ids come from `cal agenda` output)", id, err)
	}
	master, err := findMasterComponent(obj.Data, uid)
	if err != nil {
		return nil, err
	}

	changes := map[string]string{}
	if newSummary != nil {
		master.Props.SetText(ical.PropSummary, *newSummary)
		changes["summary"] = *newSummary
	}
	if newStart != nil && newEnd != nil {
		if !newEnd.After(*newStart) {
			return nil, fmt.Errorf("new end (%s) must be after new start (%s)",
				newEnd.Format(time.RFC3339), newStart.Format(time.RFC3339))
		}
		if allDay {
			master.Props.SetDate(ical.PropDateTimeStart, *newStart)
			master.Props.SetDate(ical.PropDateTimeEnd, *newEnd)
		} else {
			master.Props.SetDateTime(ical.PropDateTimeStart, *newStart)
			master.Props.SetDateTime(ical.PropDateTimeEnd, *newEnd)
		}
		changes["start"] = newStart.Format(time.RFC3339)
		changes["end"] = newEnd.Format(time.RFC3339)
	}
	if newLocation != nil {
		master.Props.SetText(ical.PropLocation, *newLocation)
		changes["location"] = *newLocation
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf(
			"no changes given — provide at least one of --summary, --start together with --end, or --location")
	}
	master.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())

	plan := &UpdatePlan{
		ID: id, Calendar: calendarID, Changes: changes,
		calendarID: calendarID, uid: uid, path: path, calObj: obj.Data,
	}
	if master.Props.Get(ical.PropRecurrenceRule) != nil {
		plan.Warning = "this event recurs — the update applies to the entire series, not a single occurrence"
	}
	return plan, nil
}

// Execute writes the prepared update.
func (p *UpdatePlan) Execute(ctx context.Context, client *caldav.Client) (*Event, error) {
	if _, err := client.PutCalendarObject(ctx, p.path, p.calObj); err != nil {
		return nil, fmt.Errorf("updating event on calendar %q: %w", p.calendarID, err)
	}

	master, err := findMasterComponent(p.calObj, p.uid)
	if err != nil {
		return nil, err
	}
	me := &ical.Event{Component: master}
	start, err := me.DateTimeStart(time.UTC)
	if err != nil {
		return nil, fmt.Errorf("parsing updated event's start time: %w", err)
	}
	ev := eventFromICal(me, p.uid, start, isAllDay(me), p.calendarID)
	return &ev, nil
}

// DeletePlan is a fully-resolved event deletion. Building one fetches the
// existing event (a read-only call, to show what would be deleted) but
// doesn't delete anything.
type DeletePlan struct {
	ID       string `json:"id"`
	Calendar string `json:"calendar"`
	Summary  string `json:"summary"`
	Start    string `json:"start"`
	Warning  string `json:"warning,omitempty"`

	calendarID string
	path       string
}

// PrepareDelete fetches the event named by id so its summary can be shown
// in a --dry-run preview or confirmation, without deleting anything.
// Deleting a recurring event's id removes the entire series, not just that
// occurrence — see docket-design.md §5's known limitations; Warning is set
// when this applies.
func PrepareDelete(ctx context.Context, client *caldav.Client, calendarID, id string, loc *time.Location) (*DeletePlan, error) {
	uid, occStart, err := parseEventID(id, loc)
	if err != nil {
		return nil, err
	}
	path := eventObjectPath(calendarID, uid)

	obj, err := client.GetCalendarObject(ctx, path)
	if err != nil {
		return nil, fmt.Errorf(
			"fetching event %q to delete: %w (event ids come from `cal agenda` output)", id, err)
	}
	master, err := findMasterComponent(obj.Data, uid)
	if err != nil {
		return nil, err
	}
	summary, _ := master.Props.Text(ical.PropSummary)

	plan := &DeletePlan{
		ID: id, Calendar: calendarID, Summary: summary, Start: occStart.Format(time.RFC3339),
		calendarID: calendarID, path: path,
	}
	if master.Props.Get(ical.PropRecurrenceRule) != nil {
		plan.Warning = "this event recurs — deleting it removes the entire series, not a single occurrence"
	}
	return plan, nil
}

// Execute deletes the prepared event.
func (p *DeletePlan) Execute(ctx context.Context, client *caldav.Client) error {
	if err := client.RemoveAll(ctx, p.path); err != nil {
		return fmt.Errorf("deleting event on calendar %q: %w", p.calendarID, err)
	}
	return nil
}
