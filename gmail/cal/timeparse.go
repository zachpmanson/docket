package cal

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseTime accepts an absolute RFC3339 timestamp or a small set of
// relative forms an agent is likely to produce: "now", "+3d"/"+2h"/"+45m",
// "today [HH:MM]", "tomorrow [HH:MM]". loc anchors relative dates and
// bare times-of-day; it should be the calendar's configured timezone
// unless --tz overrode it. See docket-design.md §5 ("echo the resolved
// absolute time... so the agent can verify the interpretation was right").
func ParseTime(input string, loc *time.Location) (time.Time, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time value")
	}

	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}

	now := time.Now().In(loc)

	if strings.EqualFold(s, "now") {
		return now, nil
	}

	if rel := relativeOffsetRE.FindStringSubmatch(s); rel != nil {
		d, err := ParseDuration(rel[1])
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(d), nil
	}

	lower := strings.ToLower(s)
	switch {
	case lower == "today" || strings.HasPrefix(lower, "today "):
		return applyTimeOfDay(now, strings.TrimSpace(s[len("today"):]))
	case lower == "tomorrow" || strings.HasPrefix(lower, "tomorrow "):
		base := now.AddDate(0, 0, 1)
		return applyTimeOfDay(base, strings.TrimSpace(s[len("tomorrow"):]))
	}

	return time.Time{}, fmt.Errorf(
		"could not parse time %q — accepted forms: RFC3339 (\"2026-08-20T14:00:00+10:00\"), "+
			"\"now\", \"+3d\"/\"+2h\"/\"+45m\", \"today [HH:MM]\", \"tomorrow [HH:MM]\"", input)
}

var relativeOffsetRE = regexp.MustCompile(`^\+([0-9]+[dhms])$`)

func applyTimeOfDay(base time.Time, hhmm string) (time.Time, error) {
	if hhmm == "" {
		return time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, base.Location()), nil
	}
	parts := strings.SplitN(hhmm, ":", 2)
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf(
			"could not parse time-of-day %q — expected HH:MM, e.g. \"tomorrow 14:00\"", hhmm)
	}
	hour, err1 := strconv.Atoi(parts[0])
	minute, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return time.Time{}, fmt.Errorf(
			"could not parse time-of-day %q — expected HH:MM with hour 0-23 and minute 0-59", hhmm)
	}
	return time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, base.Location()), nil
}

var durationPartRE = regexp.MustCompile(`([0-9]+)([dhms])`)

// ParseDuration extends time.ParseDuration with a "d" (24h day) unit, so
// flags like --within 5d and --duration 1d12h work without an agent having
// to convert days to hours itself.
func ParseDuration(input string) (time.Duration, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, fmt.Errorf("empty duration value")
	}

	matches := durationPartRE.FindAllStringSubmatch(s, -1)
	if matches == nil {
		return 0, fmt.Errorf(
			"could not parse duration %q — expected a number followed by d/h/m/s, "+
				"e.g. \"45m\", \"5d\", \"1d12h\"", input)
	}

	var reconstructed strings.Builder
	var total time.Duration
	for _, m := range matches {
		reconstructed.WriteString(m[0])
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, fmt.Errorf("invalid number in duration %q: %w", input, err)
		}
		switch m[2] {
		case "d":
			total += time.Duration(n) * 24 * time.Hour
		case "h":
			total += time.Duration(n) * time.Hour
		case "m":
			total += time.Duration(n) * time.Minute
		case "s":
			total += time.Duration(n) * time.Second
		}
	}

	if reconstructed.String() != s {
		return 0, fmt.Errorf(
			"could not parse duration %q — expected a number followed by d/h/m/s, "+
				"e.g. \"45m\", \"5d\", \"1d12h\" (no other characters)", input)
	}

	return total, nil
}

// HourRange is a parsed --hours 09:00-17:00 window, minutes since midnight.
type HourRange struct {
	StartMin int
	EndMin   int
}

var hourRangeRE = regexp.MustCompile(`^([0-9]{1,2}):([0-9]{2})-([0-9]{1,2}):([0-9]{2})$`)

// ParseHourRange parses "HH:MM-HH:MM".
func ParseHourRange(input string) (HourRange, error) {
	m := hourRangeRE.FindStringSubmatch(strings.TrimSpace(input))
	if m == nil {
		return HourRange{}, fmt.Errorf(
			"could not parse --hours %q — expected HH:MM-HH:MM, e.g. \"09:00-17:00\"", input)
	}
	sh, _ := strconv.Atoi(m[1])
	sm, _ := strconv.Atoi(m[2])
	eh, _ := strconv.Atoi(m[3])
	em, _ := strconv.Atoi(m[4])
	if sh > 23 || eh > 23 || sm > 59 || em > 59 {
		return HourRange{}, fmt.Errorf(
			"could not parse --hours %q — hour must be 0-23 and minute 0-59", input)
	}
	start := sh*60 + sm
	end := eh*60 + em
	if end <= start {
		return HourRange{}, fmt.Errorf(
			"--hours %q has an end time at or before its start time", input)
	}
	return HourRange{StartMin: start, EndMin: end}, nil
}
