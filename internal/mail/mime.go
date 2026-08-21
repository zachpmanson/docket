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

// contentID returns a part's Content-ID with the angle brackets RFC 2392
// requires in the header stripped, because the cid: URL in an html body
// carries the bare token. Returning the header verbatim would leave every
// consumer doing the same trim, and one of them getting it wrong is a broken
// image with no error anywhere.
func contentID(part *gmail.MessagePart) string {
	return strings.Trim(header(part.Headers, "Content-ID"), "<>")
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

// maxRefLen bounds the backward scan for an unterminated character
// reference: the longest name in the HTML5 named-reference table is
// "&CounterClockwiseContourIntegral;", 33 bytes with its terminator.
const maxRefLen = 33

// truncateHTML applies a byte cap to a text/html part and reports whether it
// bit. The cut is pulled back to sit after the last complete tag and the last
// complete character reference, so what a caller receives is a document that
// ends early rather than one whose tail is a fragment.
//
// The alternative — cutting at the byte the cap names — ends bodies mid-tag,
// and a parser recovering from "<a href=\"https://exa" swallows the rest of
// the document into an attribute value: the visible loss is everything after
// the cut, not just the fragment. Unclosed *elements* are left as they are,
// because every HTML parser closes those implicitly.
func truncateHTML(s string, n int) (string, bool) {
	if n >= len(s) {
		return s, false
	}
	cut := truncateAtRune(s, n)
	if i := strings.LastIndexByte(cut, '<'); i > strings.LastIndexByte(cut, '>') {
		cut = cut[:i]
	}
	if i := unterminatedRef(cut); i >= 0 {
		cut = cut[:i]
	}
	return cut, true
}

// unterminatedRef returns the index of a character reference left open at the
// end of s, or -1. A half-written "&am" renders as visible garbage, and a
// downstream sanitiser re-serialising it can turn it into something else
// again.
func unterminatedRef(s string) int {
	lo := max(len(s)-maxRefLen, 0)
	for i := len(s) - 1; i >= lo; i-- {
		switch s[i] {
		case ';', '<', '>', ' ', '\t', '\n', '\r':
			return -1
		case '&':
			return i
		}
	}
	return -1
}

// findBody walks the MIME part tree in document order and returns the first
// text/plain part and the first text/html part it finds, plus attachment
// metadata for every part carrying a filename.
//
// Both parts are returned rather than one: the plain part is the body, and
// the html part is what --html hands back untouched. For the usual
// multipart/alternative pair that means the text/plain and the text/html
// representations of the same message. Nesting is irrelevant to the choice —
// a multipart/mixed wrapping an alternative, or a multipart/related pairing
// an html part with the images it references, resolve to the same two parts.
//
// Inline images are never body candidates: a part is only considered for a
// body if its MIME type says text/plain or text/html, so the image/* members
// of a multipart/related are skipped, and appear under attachments when they
// carry a filename.
func findBody(part *gmail.MessagePart) (plain, html string, attachments []Attachment) {
	if part == nil {
		return "", "", nil
	}

	if part.Filename != "" {
		attachments = append(attachments, Attachment{
			Filename:  part.Filename,
			MimeType:  part.MimeType,
			Size:      part.Body.Size,
			PartID:    part.PartId,
			ContentID: contentID(part),
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
