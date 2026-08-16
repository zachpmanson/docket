package cal

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/emersion/go-webdav/caldav"
)

// maxSlots caps how many candidate slots FindSlot returns, so a wide-open
// calendar doesn't dump hundreds of near-identical gaps on the agent.
const maxSlots = 10

// FindSlot finds open gaps of at least duration across the given
// calendarIDs, within [windowStart, windowEnd), restricted to hours-of-day
// (in loc) on each calendar day. Built on FreeBusy per docket-design.md §5
// — this is the highest-value calendar command for an agent, turning "when
// can we meet" from several round-trips of reasoning over raw events into
// one call. Passing every calendar's id (as --calendar all does) makes the
// gaps respect busy time from all of them at once.
func FindSlot(ctx context.Context, client *caldav.Client, calendarIDs []string, duration time.Duration,
	windowStart, windowEnd time.Time, hours HourRange, loc *time.Location) ([]TimeRange, error) {

	if duration <= 0 {
		return nil, fmt.Errorf("--duration must be positive, got %s", duration)
	}
	if !windowEnd.After(windowStart) {
		return nil, fmt.Errorf("search window end (%s) is not after its start (%s)",
			windowEnd.Format(time.RFC3339), windowStart.Format(time.RFC3339))
	}

	fb, err := FreeBusy(ctx, client, calendarIDs, windowStart, windowEnd, loc)
	if err != nil {
		return nil, err
	}

	var busy [][2]time.Time
	for _, id := range calendarIDs {
		rs, err := parseBusyRanges(fb.Busy[id], loc)
		if err != nil {
			return nil, err
		}
		busy = append(busy, rs...)
	}
	busy = mergeRanges(busy)

	var slots []TimeRange
	for day := windowStart.In(loc); !day.After(windowEnd); day = day.AddDate(0, 0, 1) {
		dayStart := dayTime(day, hours.StartMin, loc)
		dayEnd := dayTime(day, hours.EndMin, loc)

		if dayStart.Before(windowStart) {
			dayStart = windowStart
		}
		if dayEnd.After(windowEnd) {
			dayEnd = windowEnd
		}
		if !dayEnd.After(dayStart) {
			continue
		}

		slots = append(slots, gapsInWindow(dayStart, dayEnd, busy, duration)...)
		if len(slots) >= maxSlots {
			return slots[:maxSlots], nil
		}
	}

	return slots, nil
}

func parseBusyRanges(ranges []TimeRange, loc *time.Location) ([][2]time.Time, error) {
	out := make([][2]time.Time, len(ranges))
	for i, r := range ranges {
		start, err := parseBusyTime(r.Start, loc)
		if err != nil {
			return nil, fmt.Errorf("parsing busy range start %q: %w", r.Start, err)
		}
		end, err := parseBusyTime(r.End, loc)
		if err != nil {
			return nil, fmt.Errorf("parsing busy range end %q: %w", r.End, err)
		}
		out[i] = [2]time.Time{start, end}
	}
	return out, nil
}

// parseBusyTime accepts either an RFC3339 timestamp or the date-only form
// (YYYY-MM-DD) FreeBusy uses for all-day events.
func parseBusyTime(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02", s, loc)
}

func mergeRanges(ranges [][2]time.Time) [][2]time.Time {
	if len(ranges) == 0 {
		return ranges
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i][0].Before(ranges[j][0]) })

	merged := [][2]time.Time{ranges[0]}
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r[0].After(last[1]) {
			merged = append(merged, r)
		} else if r[1].After(last[1]) {
			last[1] = r[1]
		}
	}
	return merged
}

func dayTime(day time.Time, minutesSinceMidnight int, loc *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc).
		Add(time.Duration(minutesSinceMidnight) * time.Minute)
}

// gapsInWindow finds every gap >= duration between busy ranges that overlap
// [windowStart, windowEnd), in order.
func gapsInWindow(windowStart, windowEnd time.Time, busy [][2]time.Time, duration time.Duration) []TimeRange {
	var slots []TimeRange
	cursor := windowStart

	for _, b := range busy {
		if b[1].Before(windowStart) || !b[0].Before(windowEnd) {
			continue
		}
		if b[0].After(cursor) && b[0].Sub(cursor) >= duration {
			slots = append(slots, TimeRange{
				Start: cursor.Format(time.RFC3339),
				End:   cursor.Add(duration).Format(time.RFC3339),
			})
		}
		if b[1].After(cursor) {
			cursor = b[1]
		}
	}

	if windowEnd.Sub(cursor) >= duration {
		slots = append(slots, TimeRange{
			Start: cursor.Format(time.RFC3339),
			End:   cursor.Add(duration).Format(time.RFC3339),
		})
	}

	return slots
}
