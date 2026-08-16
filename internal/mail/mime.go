package mail

import (
	"encoding/base64"
	"regexp"
	"strings"

	"google.golang.org/api/gmail/v1"
)

func header(headers []*gmail.MessagePartHeader, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func envelopeFromMessage(msg *gmail.Message, labels *LabelCache) Envelope {
	var headers []*gmail.MessagePartHeader
	if msg.Payload != nil {
		headers = msg.Payload.Headers
	}
	return Envelope{
		ID:       msg.Id,
		ThreadID: msg.ThreadId,
		From:     header(headers, "From"),
		To:       header(headers, "To"),
		Subject:  header(headers, "Subject"),
		Date:     header(headers, "Date"),
		Labels:   labels.Names(msg.LabelIds),
		Snippet:  msg.Snippet,
	}
}

func decodeBase64URL(data string) string {
	b, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(data)
	if err != nil {
		// Some responses include padding; retry with standard URL encoding.
		b, err = base64.URLEncoding.DecodeString(data)
		if err != nil {
			return ""
		}
	}
	return string(b)
}

var htmlTagRE = regexp.MustCompile(`(?s)<[^>]*>`)

func htmlToText(html string) string {
	text := htmlTagRE.ReplaceAllString(html, "")
	return strings.TrimSpace(text)
}

// findBody walks the MIME part tree preferring text/plain, falling back to
// text/html converted to plain text. It also collects attachment metadata
// for every part that has a filename.
func findBody(part *gmail.MessagePart) (plain, html string, attachments []Attachment) {
	if part == nil {
		return "", "", nil
	}

	if part.Filename != "" {
		attachments = append(attachments, Attachment{
			Filename: part.Filename,
			MimeType: part.MimeType,
			Size:     part.Body.Size,
			PartID:   part.PartId,
		})
	} else if part.Body != nil && part.Body.Data != "" {
		switch part.MimeType {
		case "text/plain":
			plain = decodeBase64URL(part.Body.Data)
		case "text/html":
			html = decodeBase64URL(part.Body.Data)
		}
	}

	for _, child := range part.Parts {
		p, h, a := findBody(child)
		if plain == "" {
			plain = p
		}
		if html == "" {
			html = h
		}
		attachments = append(attachments, a...)
	}

	return plain, html, attachments
}
