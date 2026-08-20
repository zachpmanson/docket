package mail

import (
	"context"
	"fmt"

	"google.golang.org/api/gmail/v1"
)

const defaultMaxBytes = 20_000

// Read fetches a single message by its stable Gmail id (the id field
// returned by Search/List — not an IMAP UID or sequence number) and returns
// its body, preferring text/plain and falling back to text/html converted
// to plain text. The body is truncated at maxBytes with Truncated=true so
// the agent knows it didn't see everything; maxBytes<=0 uses the default.
func Read(ctx context.Context, svc *gmail.Service, labels *LabelCache, id string, maxBytes int) (*Message, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
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
	if len(body) > maxBytes {
		body = body[:maxBytes]
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
