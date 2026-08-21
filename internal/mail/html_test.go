package mail

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
)

// Every fixture below is invented. The shapes are real MIME shapes; the
// names, addresses and links are not anyone's.
const (
	plainSource = "Approved. Costs & timing as discussed.\n"
	htmlSource  = `<div><p>Approved. Costs &amp; timing as discussed.</p>` +
		`<p>See <a href="https://app.example.com/admin/editor/rate-review?id=2547678&amp;tab=notes">this review</a>.</p></div>`
)

func textPart(body string) *gmail.MessagePart {
	return &gmail.MessagePart{MimeType: "text/plain", Body: partBody(body)}
}

func htmlPart(body string) *gmail.MessagePart {
	return &gmail.MessagePart{MimeType: "text/html", Body: partBody(body)}
}

func inlineImagePart() *gmail.MessagePart {
	return &gmail.MessagePart{
		MimeType: "image/png",
		PartId:   "1.2",
		Filename: "logo.png",
		Body:     &gmail.MessagePartBody{Size: 4096, AttachmentId: "att-1"},
	}
}

func multipart(mimeType string, parts ...*gmail.MessagePart) *gmail.MessagePart {
	return &gmail.MessagePart{MimeType: mimeType, Parts: parts}
}

func readPayload(t *testing.T, payload *gmail.MessagePart, opts ReadOptions) *Message {
	t.Helper()
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.payloads["m1"] = payload
	msg, err := Read(context.Background(), f.service(t), emptyLabels(), "m1", opts)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return msg
}

// TestPartSelection pins which part becomes which field for every multipart
// shape real mail arrives in. Getting this wrong is silent: a caller receives
// a body, just not the message's.
func TestPartSelection(t *testing.T) {
	cases := []struct {
		name     string
		payload  *gmail.MessagePart
		wantHTML string
		status   string
		// wantBody is the plain-text body field, which falls back to the
		// html part rendered as text when there is no text/plain part.
		wantBody string
	}{{
		name:     "text/plain only",
		payload:  textPart(plainSource),
		wantHTML: "",
		status:   HTMLNone,
		wantBody: plainSource,
	}, {
		name:     "text/html only",
		payload:  htmlPart(htmlSource),
		wantHTML: htmlSource,
		status:   HTMLPresent,
		wantBody: "Approved. Costs & timing as discussed.See this review.",
	}, {
		name:     "multipart/alternative with both",
		payload:  multipart("multipart/alternative", textPart(plainSource), htmlPart(htmlSource)),
		wantHTML: htmlSource,
		status:   HTMLPresent,
		wantBody: plainSource,
	}, {
		name:     "multipart/related with an inline image",
		payload:  multipart("multipart/related", htmlPart(htmlSource), inlineImagePart()),
		wantHTML: htmlSource,
		status:   HTMLPresent,
		wantBody: "Approved. Costs & timing as discussed.See this review.",
	}, {
		name: "multipart/mixed wrapping an alternative",
		payload: multipart("multipart/mixed",
			multipart("multipart/alternative", textPart(plainSource), htmlPart(htmlSource)),
			&gmail.MessagePart{
				MimeType: "application/pdf", PartId: "2", Filename: "quote.pdf",
				Body: &gmail.MessagePartBody{Size: 9000, AttachmentId: "att-2"},
			}),
		wantHTML: htmlSource,
		status:   HTMLPresent,
		wantBody: plainSource,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := readPayload(t, tc.payload, ReadOptions{MaxBytes: NoMaxBytes, IncludeHTML: true})
			if msg.BodyHTML != tc.wantHTML {
				t.Errorf("body_html = %q, want %q", msg.BodyHTML, tc.wantHTML)
			}
			if msg.HTMLStatus != tc.status {
				t.Errorf("html_status = %q, want %q", msg.HTMLStatus, tc.status)
			}
			if msg.Body != tc.wantBody {
				t.Errorf("body = %q, want %q", msg.Body, tc.wantBody)
			}
		})
	}
}

// TestHTMLIsReturnedUnmodified is the whole point of the flag: a consumer
// sanitises at render time and cannot do that with markup docket has already
// rewritten. The href is the specific loss that motivated it — flattening to
// text keeps the link words and drops the target.
func TestHTMLIsReturnedUnmodified(t *testing.T) {
	msg := readPayload(t, multipart("multipart/alternative", textPart(plainSource), htmlPart(htmlSource)),
		ReadOptions{MaxBytes: NoMaxBytes, IncludeHTML: true})

	if msg.BodyHTML != htmlSource {
		t.Fatalf("body_html = %q, want the source byte for byte", msg.BodyHTML)
	}
	if !strings.Contains(msg.BodyHTML, `href="https://app.example.com/admin/editor/rate-review?id=2547678&amp;tab=notes"`) {
		t.Errorf("the link target did not survive: %q", msg.BodyHTML)
	}
	if strings.Contains(msg.Body, "app.example.com") {
		t.Errorf("body claims to be plain text but carries markup URLs: %q", msg.Body)
	}
}

func TestNoHTMLPartIsSignalled(t *testing.T) {
	// A text-only message is normal. The caller has to be able to tell that
	// from a flag that did nothing, and an empty body_html says both.
	msg := readPayload(t, textPart(plainSource), ReadOptions{MaxBytes: NoMaxBytes, IncludeHTML: true})

	if msg.HTMLStatus != HTMLNone {
		t.Errorf("html_status = %q, want %q", msg.HTMLStatus, HTMLNone)
	}
	if msg.BodyHTML != "" {
		t.Errorf("body_html = %q for a message with no html part, want empty", msg.BodyHTML)
	}
}

func TestHTMLStatusIsAbsentWhenNotRequested(t *testing.T) {
	// Absent, not "none": a stored response with html_status:"none" means the
	// message was checked and had no html part. Emitting it for a read that
	// never looked would make that claim falsely.
	msg := readPayload(t, multipart("multipart/alternative", textPart(plainSource), htmlPart(htmlSource)),
		ReadOptions{MaxBytes: NoMaxBytes})

	if msg.HTMLStatus != "" || msg.BodyHTML != "" {
		t.Errorf("html_status = %q, body_html = %q without --html, want both empty", msg.HTMLStatus, msg.BodyHTML)
	}
	got := marshal(t, msg)
	for _, key := range []string{"html_status", "body_html", "html_truncated"} {
		if strings.Contains(got, key) {
			t.Errorf("JSON carries %q without --html: %s", key, got)
		}
	}
}

// TestBodyHTMLIsNotDecoded guards the other side of the entity contract:
// body is decoded because it claims to be text, and body_html is not because
// it claims to be the part as sent. A sender writing about html escaping means
// the "&amp;amp;" they typed.
func TestBodyHTMLIsNotDecoded(t *testing.T) {
	source := `<p>In html an ampersand is written &amp;amp; and a space is &amp;nbsp;.</p>`

	msg := readPayload(t, htmlPart(source), ReadOptions{MaxBytes: NoMaxBytes, IncludeHTML: true})

	if msg.BodyHTML != source {
		t.Errorf("body_html = %q, want the source byte for byte", msg.BodyHTML)
	}
	if msg.Body != "In html an ampersand is written &amp; and a space is &nbsp;." {
		t.Errorf("body = %q, want one round of decoding and no more", msg.Body)
	}
}

func TestHTMLTruncationCutsAtATagBoundary(t *testing.T) {
	// A cut inside a tag is not recoverable by a parser: the rest of the
	// document disappears into an attribute value.
	source := `<div><p>` + strings.Repeat("filler text ", 40) +
		`</p><a href="https://example.com/a-long-enough-target-to-straddle">link</a></div>`

	for _, n := range []int{1, 8, 100, 200, 300, 480, 500, 520, len(source) - 1} {
		msg := readPayload(t, htmlPart(source), ReadOptions{MaxBytes: n, IncludeHTML: true})
		if len(msg.BodyHTML) > n {
			t.Errorf("cap %d: body_html is %d bytes", n, len(msg.BodyHTML))
		}
		if !msg.HTMLTruncated {
			t.Errorf("cap %d: html_truncated = false on a cut body", n)
		}
		if i := strings.LastIndexByte(msg.BodyHTML, '<'); i > strings.LastIndexByte(msg.BodyHTML, '>') {
			t.Errorf("cap %d: body_html ends inside a tag: %q", n, msg.BodyHTML[i:])
		}
	}
}

func TestHTMLTruncationDoesNotSplitACharacterReference(t *testing.T) {
	// "&am" is visible garbage, and a sanitiser re-serialising it can turn it
	// into something else again.
	source := `<p>a b c&amp;defghijklmnop</p>`

	for n := 1; n < len(source); n++ {
		msg := readPayload(t, htmlPart(source), ReadOptions{MaxBytes: n, IncludeHTML: true})
		if i := strings.LastIndexByte(msg.BodyHTML, '&'); i >= 0 && !strings.Contains(msg.BodyHTML[i:], ";") {
			t.Fatalf("cap %d: body_html ends in an unterminated reference: %q", n, msg.BodyHTML)
		}
	}
}

func TestTheTwoTruncationFlagsAreIndependent(t *testing.T) {
	// The cap is per body, and an html part is routinely an order of
	// magnitude larger than the text beside it. One shared flag would report
	// the text cut when it was whole.
	plain := "Short confirmation.\n"
	markup := `<div><p>Short confirmation.</p>` + strings.Repeat(`<span class="pad">x</span>`, 200) + `</div>`

	msg := readPayload(t, multipart("multipart/alternative", textPart(plain), htmlPart(markup)),
		ReadOptions{MaxBytes: 200, IncludeHTML: true})

	if msg.Truncated {
		t.Errorf("truncated = true for a %d-byte text body under a 200-byte cap", len(plain))
	}
	if msg.Body != plain {
		t.Errorf("body = %q, want it whole", msg.Body)
	}
	if !msg.HTMLTruncated {
		t.Errorf("html_truncated = false for a %d-byte html part under a 200-byte cap", len(markup))
	}
}

func TestHTMLIsWholeWithNoCap(t *testing.T) {
	source := `<div>` + strings.Repeat(`<p>paragraph</p>`, 5000) + `</div>`

	msg := readPayload(t, htmlPart(source), ReadOptions{MaxBytes: NoMaxBytes, IncludeHTML: true})

	if msg.BodyHTML != source {
		t.Errorf("body_html is %d bytes, want the full %d", len(msg.BodyHTML), len(source))
	}
	if msg.HTMLTruncated {
		t.Errorf("html_truncated = true with no cap applied")
	}
}

func TestHTMLUnderTheCapIsNotFlagged(t *testing.T) {
	// Off-by-one here sends a caller after markup it already has in full.
	msg := readPayload(t, htmlPart(htmlSource), ReadOptions{MaxBytes: len(htmlSource), IncludeHTML: true})

	if msg.HTMLTruncated {
		t.Errorf("html_truncated = true when the cap equals the part length")
	}
	if msg.BodyHTML != htmlSource {
		t.Errorf("body_html was altered at a cap equal to its length")
	}
}

func threadFixture(t *testing.T, opts ReadOptions) *Thread {
	t.Helper()
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1", "m2"}}})
	f.payloads["m1"] = multipart("multipart/alternative", textPart(plainSource), htmlPart(htmlSource))
	f.payloads["m2"] = textPart("Noted, thanks.\n")
	f.threads["t-1"] = []string{"m1", "m2"}

	thread, err := GetThread(context.Background(), f.service(t), emptyLabels(), "t-1", opts)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	return thread
}

func TestThreadStaysEnvelopeOnlyByDefault(t *testing.T) {
	// A conversation's worth of bodies is the largest response docket can
	// produce, so it stays opt-in — and the default output has to stay
	// byte-identical for callers already parsing it.
	thread := threadFixture(t, ReadOptions{MaxBytes: DefaultMaxBytes})

	got := marshal(t, thread)
	for _, key := range []string{`"body"`, `"body_html"`, `"truncated"`, `"html_status"`} {
		if strings.Contains(got, key) {
			t.Errorf("default thread output carries %s: %s", key, got)
		}
	}
}

func TestThreadWithHTMLCarriesEveryMessagesBodies(t *testing.T) {
	thread := threadFixture(t, ReadOptions{MaxBytes: NoMaxBytes, IncludeHTML: true})

	if len(thread.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(thread.Messages))
	}
	if thread.Messages[0].BodyHTML != htmlSource {
		t.Errorf("first message body_html = %q, want the source", thread.Messages[0].BodyHTML)
	}
	if thread.Messages[0].HTMLStatus != HTMLPresent {
		t.Errorf("first message html_status = %q, want %q", thread.Messages[0].HTMLStatus, HTMLPresent)
	}
	// A text-only reply mid-conversation is why the text body rides along:
	// there would otherwise be nothing to render for it.
	if thread.Messages[1].HTMLStatus != HTMLNone {
		t.Errorf("second message html_status = %q, want %q", thread.Messages[1].HTMLStatus, HTMLNone)
	}
	if thread.Messages[1].Body != "Noted, thanks.\n" {
		t.Errorf("second message body = %q, want the text part", thread.Messages[1].Body)
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return string(b)
}
