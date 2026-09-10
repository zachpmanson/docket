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

// ReadOptions are the per-call knobs for Read and GetThread.
//
// MaxBytes takes no default here, unlike ListOptions.Limit: NoMaxBytes is a
// caller-visible sentinel meaning "all of it", so the zero value is that
// request and substituting DefaultMaxBytes for it would truncate a caller
// who asked for the opposite. The command layer passes the default in.
//
// The cap applies to each body independently, not to their total. A message
// whose text sits under the cap and whose markup does not comes back with
// complete text and truncated markup, each flagged for what it is.
type ReadOptions struct {
	MaxBytes    int
	IncludeHTML bool
}

// Read fetches a single message by its stable Gmail id (the id field
// returned by Search/List — not an IMAP UID or sequence number) and returns
// its body, preferring text/plain and falling back to text/html converted
// to plain text. The body is truncated at opts.MaxBytes with Truncated=true so
// the agent knows it didn't see everything; a MaxBytes of NoMaxBytes returns
// the whole body.
//
// With IncludeHTML the raw text/html part rides along in BodyHTML, and
// HTMLStatus says whether the message had one at all.
func Read(ctx context.Context, svc *gmail.Service, labels *LabelCache, id string, opts ReadOptions) (*Message, error) {
	if opts.MaxBytes < 0 {
		return nil, fmt.Errorf("--max-bytes %d: %w", opts.MaxBytes, ErrNegativeMaxBytes)
	}

	msg, err := svc.Users.Messages.Get(meUser, id).Format("full").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf(
			"fetching message %q: %w (docket mail ids come from `mail search`/`mail list` output — "+
				"confirm this id was copied from there, not an IMAP UID or sequence number)", id, err)
	}

	plain, markup, attachments := findBody(msg.Payload)
	r := render(plain, markup, opts)

	m := &Message{
		Envelope:      envelopeFromMessage(msg, labels),
		Body:          r.body,
		Truncated:     r.truncated,
		HTMLStatus:    r.status,
		BodyHTML:      r.html,
		HTMLTruncated: r.htmlTruncated,
		Attachments:   attachments,
	}
	return m, nil
}

// rendered is one message's body fields after the cap and the HTML request
// have been applied.
type rendered struct {
	body          string
	truncated     bool
	status        string
	html          string
	htmlTruncated bool
}

// render turns one message's parts into the body fields Read and GetThread
// both report, so the two commands cannot drift on what --html and
// --max-bytes mean.
func render(plain, markup string, opts ReadOptions) rendered {
	r := rendered{body: plain}
	if r.body == "" {
		r.body = htmlToText(markup)
	}
	if opts.MaxBytes != NoMaxBytes && len(r.body) > opts.MaxBytes {
		r.body = truncateAtRune(r.body, opts.MaxBytes)
		r.truncated = true
	}

	if !opts.IncludeHTML {
		return r
	}
	// An empty text/html part and no text/html part are one answer to a
	// caller asking what to render, and Gmail does not distinguish them in a
	// response either: a part with no data carries no data field.
	if markup == "" {
		r.status = HTMLNone
		return r
	}
	r.status = HTMLPresent
	if opts.MaxBytes == NoMaxBytes {
		r.html = markup
		return r
	}
	r.html, r.htmlTruncated = truncateHTML(markup, opts.MaxBytes)
	return r
}

// GetThread fetches every message in a thread by its stable Gmail thread id
// (threadId from an Envelope), returning envelope-only summaries — the
// agent calls Read on a specific message id if it needs that message's body.
//
// With IncludeHTML it fetches full format instead and every message carries
// its bodies. The text body comes along with the markup because it is in the
// same response and a text-only message in the middle of a conversation has
// nothing else to render; a caller would otherwise have to fall back to a
// per-message Read for exactly those.
func GetThread(ctx context.Context, svc *gmail.Service, labels *LabelCache, threadID string, opts ReadOptions) (*Thread, error) {
	if opts.MaxBytes < 0 {
		return nil, fmt.Errorf("--max-bytes %d: %w", opts.MaxBytes, ErrNegativeMaxBytes)
	}

	format := "metadata"
	if opts.IncludeHTML {
		format = "full"
	}
	call := svc.Users.Threads.Get(meUser, threadID).Format(format)
	if format == "metadata" {
		call = call.MetadataHeaders(metadataHeaders...)
	}
	t, err := call.Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf(
			"fetching thread %q: %w (thread ids come from an Envelope's thread_id field, "+
				"returned by `mail search`/`mail list`/`mail read`)", threadID, err)
	}

	messages := make([]ThreadMessage, len(t.Messages))
	for i, msg := range t.Messages {
		messages[i] = ThreadMessage{Envelope: envelopeFromMessage(msg, labels)}
		if !opts.IncludeHTML {
			continue
		}
		plain, markup, _ := findBody(msg.Payload)
		r := render(plain, markup, opts)
		messages[i].Body = r.body
		messages[i].Truncated = r.truncated
		messages[i].HTMLStatus = r.status
		messages[i].BodyHTML = r.html
		messages[i].HTMLTruncated = r.htmlTruncated
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
