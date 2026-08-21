package mail

import (
	"encoding/base64"
	"html"
	"regexp"
	"strings"

	"google.golang.org/api/gmail/v1"
)

// metadataHeaders is the set of headers requested for envelope-only fetches
// (Format("metadata")). It must cover every field envelopeFromMessage reads
// when the caller uses metadata format rather than "full".
var metadataHeaders = []string{
	"From", "To", "Cc", "Subject", "Date",
	"Message-ID", "In-Reply-To", "References",
}

func header(headers []*gmail.MessagePartHeader, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// headerValues returns the whitespace-split values of every header with the
// given name. Used for References, whose value is a whitespace-separated list
// of RFC822 Message-IDs (possibly folded across multiple physical lines).
func headerValues(headers []*gmail.MessagePartHeader, name string) []string {
	var vals []string
	for _, h := range headers {
		if !strings.EqualFold(h.Name, name) {
			continue
		}
		for _, part := range strings.Fields(h.Value) {
			if part != "" {
				vals = append(vals, part)
			}
		}
	}
	return vals
}

func envelopeFromMessage(msg *gmail.Message, labels *LabelCache) Envelope {
	var headers []*gmail.MessagePartHeader
	if msg.Payload != nil {
		headers = msg.Payload.Headers
	}
	return Envelope{
		ID:         msg.Id,
		ThreadID:   msg.ThreadId,
		From:       header(headers, "From"),
		To:         header(headers, "To"),
		Cc:         header(headers, "Cc"),
		Subject:    header(headers, "Subject"),
		Date:       header(headers, "Date"),
		MessageID:  header(headers, "Message-ID"),
		InReplyTo:  header(headers, "In-Reply-To"),
		References: headerValues(headers, "References"),
		Labels:     labels.Names(msg.LabelIds),
		Snippet:    msg.Snippet,
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

// scriptStyleRE matches a <script> or <style> element together with its
// content. Stripping only the tags would leave the CSS or JS between them
// in the plain-text body, and in HTML-only marketing mail that payload is
// routinely larger than the prose it surrounds.
var scriptStyleRE = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>`)

var htmlTagRE = regexp.MustCompile(`(?s)<[^>]*>`)

// htmlToText renders a text/html part as the plain-text body, for a message
// that carries no text/plain part of its own.
//
// Character references are resolved here and nowhere else. A caller cannot
// do it afterwards: a body that legitimately contains the literal text
// "&amp;" — a plain-text part quoting HTML source, or a URL a sender typed
// by hand — must survive untouched, and only this path knows that what it
// holds is markup. A blanket unescape applied to the body field would
// corrupt exactly those messages.
//
// The order is load-bearing. Tags are stripped first, references second:
// unescaping first would turn a source "&lt;div&gt;" into "<div>", which the
// tag strip would then delete, silently removing text the message displayed.
func htmlToText(markup string) string {
	text := scriptStyleRE.ReplaceAllString(markup, "")
	text = htmlTagRE.ReplaceAllString(text, "")
	return strings.TrimSpace(html.UnescapeString(text))
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
