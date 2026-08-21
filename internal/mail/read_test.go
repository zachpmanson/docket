package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// syntheticTrail stands in for a long forwarded trail: the oldest quoted
// material sits at the END, which is what a cap removes first.
func syntheticTrail(size int) string {
	const tail = "\n> > On 12 Nov 2025, Priya Raman <priya@example.net> wrote:\n> > the original spec is attached"
	head := strings.Repeat("recent reply text\n", 1+size/len("recent reply text\n"))
	return head[:size-len(tail)] + tail
}

func readFixture(t *testing.T, body string, maxBytes int) (*Message, error) {
	t.Helper()
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.bodies["m1"] = body
	return Read(context.Background(), f.service(t), emptyLabels(), "m1", maxBytes)
}

func TestReadNoMaxBytesReturnsTheWholeBody(t *testing.T) {
	// The sentinel exists so a caller reconstructing history does not have to
	// guess a number big enough; truncation takes the oldest quoted material
	// first, which is exactly what such a caller came for.
	body := syntheticTrail(60_000)

	msg, err := readFixture(t, body, NoMaxBytes)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if msg.Body != body {
		t.Errorf("body is %d bytes, want the full %d", len(msg.Body), len(body))
	}
	if msg.Truncated {
		t.Errorf("truncated = true with no cap applied")
	}
	if !strings.Contains(msg.Body, "the original spec is attached") {
		t.Errorf("the oldest quoted material is missing from an uncapped read")
	}
}

func TestReadCapAppliesAndIsReported(t *testing.T) {
	body := syntheticTrail(60_000)

	msg, err := readFixture(t, body, 1_000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msg.Body) > 1_000 {
		t.Errorf("body is %d bytes, want at most 1000", len(msg.Body))
	}
	if !msg.Truncated {
		t.Errorf("truncated = false after a cap was applied; a caller has no other way to tell")
	}
}

func TestReadUnderTheCapIsNotMarkedTruncated(t *testing.T) {
	msg, err := readFixture(t, "short body", DefaultMaxBytes)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if msg.Truncated {
		t.Errorf("truncated = true for a body well under the cap")
	}
	if msg.Body != "short body" {
		t.Errorf("body = %q, want it unchanged", msg.Body)
	}
}

func TestReadCapExactlyAtBodyLengthDoesNotTruncate(t *testing.T) {
	// Off-by-one here would report truncation on a complete body, sending a
	// caller after a page that does not exist.
	body := "exactly this long"

	msg, err := readFixture(t, body, len(body))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if msg.Truncated {
		t.Errorf("truncated = true when the cap equals the body length")
	}
}

func TestReadRejectsANegativeCap(t *testing.T) {
	// Reading a negative as "unlimited" would collide with the sentinel, and
	// reading it as "the default" would truncate a caller plainly reaching
	// for the opposite.
	_, err := readFixture(t, "body", -1)
	if !errors.Is(err, ErrNegativeMaxBytes) {
		t.Fatalf("err = %v, want ErrNegativeMaxBytes", err)
	}
}

func TestReadTruncationKeepsValidUTF8(t *testing.T) {
	// A cap landing mid-rune would put a replacement character in the last
	// visible characters of the body the caller did receive.
	body := strings.Repeat("日", 100)

	msg, err := readFixture(t, body, 10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !utf8.ValidString(msg.Body) {
		t.Errorf("truncated body is not valid UTF-8: %q", msg.Body)
	}
	if !msg.Truncated {
		t.Errorf("truncated = false after a cap was applied")
	}
}
