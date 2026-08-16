package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/mail"
	"strings"

	"google.golang.org/api/gmail/v1"
)

// SendPlan is a fully-resolved, ready-to-send message. Building one never
// mutates anything — it's safe to construct and show as a --dry-run
// preview before the caller decides whether to Execute it.
type SendPlan struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`

	raw      string
	threadID string
}

// PrepareSend validates a send request and builds the raw RFC 5322 message,
// without sending anything.
func PrepareSend(to, subject, body string) (*SendPlan, error) {
	if _, err := mail.ParseAddressList(to); err != nil {
		return nil, fmt.Errorf(
			"--to %q is not a valid address list: %w (expected e.g. \"a@example.com\" "+
				"or \"a@example.com, b@example.com\")", to, err)
	}
	raw, err := buildRawMessage(rawMessageOptions{To: to, Subject: subject, Body: body})
	if err != nil {
		return nil, err
	}
	return &SendPlan{To: to, Subject: subject, Body: body, raw: raw}, nil
}

// PrepareReply fetches the message being replied to (a read-only call) and
// builds a threaded raw message (In-Reply-To/References + Gmail's
// ThreadId), without sending anything.
func PrepareReply(ctx context.Context, svc *gmail.Service, id, body string) (*SendPlan, error) {
	original, err := svc.Users.Messages.Get(meUser, id).
		Format("metadata").
		MetadataHeaders("Message-Id", "References", "Subject", "From").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf(
			"fetching original message %q to reply to: %w (ids come from `mail search`/"+
				"`mail list`/`mail thread` output)", id, err)
	}

	h := original.Payload.Headers
	messageID := header(h, "Message-Id")
	references := header(h, "References")
	if references == "" {
		references = messageID
	} else if messageID != "" {
		references = references + " " + messageID
	}
	subject := header(h, "Subject")
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	to := header(h, "From")
	if to == "" {
		return nil, fmt.Errorf("original message %q has no From header to reply to", id)
	}

	raw, err := buildRawMessage(rawMessageOptions{
		To: to, Subject: subject, Body: body,
		InReplyTo: messageID, References: references,
	})
	if err != nil {
		return nil, err
	}

	return &SendPlan{To: to, Subject: subject, Body: body, raw: raw, threadID: original.ThreadId}, nil
}

// Execute sends a prepared message.
func (p *SendPlan) Execute(ctx context.Context, svc *gmail.Service, labels *LabelCache) (*Envelope, error) {
	sent, err := svc.Users.Messages.Send(meUser, &gmail.Message{
		Raw:      p.raw,
		ThreadId: p.threadID,
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sending message: %w", err)
	}
	return fetchEnvelope(ctx, svc, labels, sent.Id)
}

// LabelPlan is a fully-resolved label add/remove request.
type LabelPlan struct {
	MessageID    string   `json:"message_id"`
	AddLabels    []string `json:"add_labels,omitempty"`
	RemoveLabels []string `json:"remove_labels,omitempty"`

	addIDs, removeIDs []string
}

// PrepareLabel resolves label names to ids (read-only — LabelCache is
// already loaded) without modifying anything.
func PrepareLabel(labels *LabelCache, messageID string, addNames, removeNames []string) (*LabelPlan, error) {
	addIDs, err := resolveLabelNames(labels, addNames)
	if err != nil {
		return nil, err
	}
	removeIDs, err := resolveLabelNames(labels, removeNames)
	if err != nil {
		return nil, err
	}
	return &LabelPlan{
		MessageID: messageID, AddLabels: addNames, RemoveLabels: removeNames,
		addIDs: addIDs, removeIDs: removeIDs,
	}, nil
}

func resolveLabelNames(labels *LabelCache, names []string) ([]string, error) {
	ids := make([]string, 0, len(names))
	for _, name := range names {
		id, ok := labels.ID(name)
		if !ok {
			return nil, fmt.Errorf("no label named %q; known labels: %v", name, labels.AllNames())
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Execute applies a prepared label change.
func (p *LabelPlan) Execute(ctx context.Context, svc *gmail.Service, labels *LabelCache) (*Envelope, error) {
	_, err := svc.Users.Messages.Modify(meUser, p.MessageID, &gmail.ModifyMessageRequest{
		AddLabelIds:    p.addIDs,
		RemoveLabelIds: p.removeIDs,
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf(
			"modifying labels on message %q: %w (ids come from `mail search`/`mail list` output)",
			p.MessageID, err)
	}
	return fetchEnvelope(ctx, svc, labels, p.MessageID)
}

func fetchEnvelope(ctx context.Context, svc *gmail.Service, labels *LabelCache, id string) (*Envelope, error) {
	msg, err := svc.Users.Messages.Get(meUser, id).
		Format("metadata").
		MetadataHeaders("From", "To", "Subject", "Date").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("fetching message %q after write: %w", id, err)
	}
	env := envelopeFromMessage(msg, labels)
	return &env, nil
}

type rawMessageOptions struct {
	To, Subject, Body     string
	InReplyTo, References string
}

func buildRawMessage(o rawMessageOptions) (string, error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "To: %s\r\n", o.To)
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", o.Subject))
	if o.InReplyTo != "" {
		fmt.Fprintf(&buf, "In-Reply-To: %s\r\n", o.InReplyTo)
	}
	if o.References != "" {
		fmt.Fprintf(&buf, "References: %s\r\n", o.References)
	}
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	buf.WriteString("\r\n")
	buf.WriteString(o.Body)

	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(buf.Bytes()), nil
}
