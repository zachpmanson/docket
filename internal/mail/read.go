package mail

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"google.golang.org/api/gmail/v1"
)

// DefaultMaxBytes caps a body when the caller does not say otherwise.
// NoMaxBytes is the sentinel meaning "no cap at all": truncation happens at
// the END of a body, and the end of a forward holds the OLDEST quoted
// material, so a caller reconstructing history needs a way to ask for all of
// it that does not involve guessing a number large enough.
const (
	DefaultMaxBytes = 20_000
	NoMaxBytes      = 0
)

// ErrNegativeMaxBytes marks a negative cap as the caller's mistake rather
// than something to interpret. Reading it as "unlimited" would collide with
// the explicit sentinel, and reading it as "the default" would silently
// truncate a caller who was plainly reaching for the opposite.
var ErrNegativeMaxBytes = errors.New("--max-bytes must be 0 (unlimited) or a positive byte count")

// Read fetches a single message by its stable Gmail id (the id field
// returned by Search/List — not an IMAP UID or sequence number) and returns
// its body, preferring text/plain and falling back to text/html converted
// to plain text. The body is truncated at maxBytes with Truncated=true so
// the agent knows it didn't see everything; maxBytes of NoMaxBytes returns
// the whole body.
func Read(ctx context.Context, svc *gmail.Service, labels *LabelCache, id string, maxBytes int) (*Message, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("--max-bytes %d: %w", maxBytes, ErrNegativeMaxBytes)
	}

	msg, err := svc.Users.Messages.Get(meUser, id).Format("full").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf(
			"fetching message %q: %w (docket mail ids come from `mail search`/`mail list` output — "+
				"confirm this id was copied from there, not an IMAP UID or sequence number)", id, err)
	}

	plain, html, attachments := findBody(msg.Payload)
	body := plain
	if body == "" {
		body = htmlToText(html)
	}

	truncated := false
	if maxBytes != NoMaxBytes && len(body) > maxBytes {
		body = truncateAtRune(body, maxBytes)
		truncated = true
	}

	m := &Message{
		Envelope:    envelopeFromMessage(msg, labels),
		Body:        body,
		Truncated:   truncated,
		Attachments: attachments,
	}
	return m, nil
}

// Thread fetches every message in a thread by its stable Gmail thread id
// (threadId from an Envelope), returning envelope-only summaries — the
// agent calls Read on a specific message id if it needs that message's body.
func GetThread(ctx context.Context, svc *gmail.Service, labels *LabelCache, threadID string) (*Thread, error) {
	t, err := svc.Users.Threads.Get(meUser, threadID).Format("metadata").
		MetadataHeaders(metadataHeaders...).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf(
			"fetching thread %q: %w (thread ids come from an Envelope's thread_id field, "+
				"returned by `mail search`/`mail list`/`mail read`)", threadID, err)
	}

	messages := make([]Envelope, len(t.Messages))
	for i, msg := range t.Messages {
		messages[i] = envelopeFromMessage(msg, labels)
	}
	return &Thread{ThreadID: threadID, Messages: messages}, nil
}

// truncateAtRune cuts s to at most n bytes without splitting the rune that
// straddles the boundary — a half rune would be re-encoded as U+FFFD in the
// JSON envelope, corrupting the last visible characters of the body a caller
// did receive.
func truncateAtRune(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
