package cal

import (
	"testing"
	"time"
)

func TestParseDate(t *testing.T) {
	loc, err := time.LoadLocation("Australia/Melbourne")
	if err != nil {
		t.Fatal(err)
	}

	got, err := ParseDate("2026-10-09", loc)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, time.October, 9, 0, 0, 0, 0, loc)
	if !got.Equal(want) || got.Location() != loc {
		t.Fatalf("ParseDate() = %v (%s), want %v (%s)", got, got.Location(), want, loc)
	}

	for _, input := range []string{"2026-2-09", "2026-10-09T00:00:00Z", "tomorrow", "2026-02-30"} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseDate(input, loc); err == nil {
				t.Fatalf("ParseDate(%q) unexpectedly succeeded", input)
			}
		})
	}
}
