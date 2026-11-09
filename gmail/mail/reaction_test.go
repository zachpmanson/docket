package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
)

// All source identities, references and fallback text here are invented.
func reactionPart(body string) *gmail.MessagePart {
	return &gmail.MessagePart{MimeType: gmailReactionMIME, PartId: "reaction-part", Body: partBody(body)}
}

func reactionWithTarget(p *gmail.MessagePart) *gmail.MessagePart {
	p.Headers = append(p.Headers, &gmail.MessagePartHeader{Name: "In-Reply-To", Value: "<target@example.test>"})
	return p
}

func validReaction() *gmail.MessagePart {
	return reactionPart(`{"version":1,"emoji":"❤️"}`)
}

func readReactionPayload(t *testing.T, p *gmail.MessagePart, opts ReadOptions) *Message {
	t.Helper()
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.payloads["m1"] = p
	f.headers["m1"] = p.Headers
	m, err := Read(context.Background(), f.service(t), emptyLabels(), "m1", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.attachmentRequests) != 0 {
		t.Fatal("inspection fetched external bodies")
	}
	return m
}

func TestReactionExactEmojiSequences(t *testing.T) {
	if len(reactionEmoji) != 3781 {
		t.Fatalf("pinned table has %d entries", len(reactionEmoji))
	}
	for _, emoji := range []string{"❤️", "👍🏽", "👩‍💻", "👨‍👩‍👧‍👦", "🇳🇿", "1️⃣", "😀"} {
		t.Run(emoji, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"version": 1, "emoji": emoji})
			r := inspectReactions(reactionWithTarget(reactionPart(string(b))))
			if r.Status != ReactionStatusCandidate || r.TargetMessageID != "<target@example.test>" || r.Layout != "top-level" {
				t.Fatalf("inspection: %+v", r)
			}
			c := r.Candidates[0]
			if c.Emoji != emoji || !c.BodyComplete || c.Body != string(b) || c.Version != 1 {
				t.Fatalf("sequence/bytes changed: %+v", c)
			}
			encoded, _ := json.Marshal(r)
			var decoded ReactionInspection
			if err := json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(*r, decoded) {
				t.Fatalf("JSON round trip: %v, %+v", err, decoded)
			}
		})
	}
	for _, emoji := range []string{"", "a", "á", "1", "©", "♥", "❤", "🏽", "❤️❤️", "👍 🥳", " ❤️", "👩‍"} {
		b, _ := json.Marshal(map[string]any{"version": 1, "emoji": emoji})
		r := inspectReactions(reactionWithTarget(reactionPart(string(b))))
		if r.Status != ReactionMalformed || r.Candidates[0].Emoji != "" {
			t.Errorf("accepted non-table sequence %q: %+v", emoji, r)
		}
	}
	if reactionEmoji["\U0001FAEA"] { // not present in the pinned Emoji 16.0 set
		t.Fatal("future sequence unexpectedly supported")
	}
}

func TestReactionJSONValidation(t *testing.T) {
	for _, tc := range []struct{ body, status string }{
		{`{"version":1,"emoji":"❤️","extension":{"ok":true}}`, ReactionStatusCandidate},
		{`{"version":2,"emoji":"❤️"}`, ReactionUnsupported},
		{`{"version":0,"emoji":"❤️"}`, ReactionUnsupported},
		{`{"version":"1","emoji":"❤️"}`, ReactionMalformed},
		{`{"version":1.0,"emoji":"❤️"}`, ReactionMalformed},
		{`{"version":null,"emoji":"❤️"}`, ReactionMalformed},
		{`{"emoji":"❤️"}`, ReactionMalformed},
		{`{"version":1}`, ReactionMalformed},
		{`{"version":1,"emoji":null}`, ReactionMalformed},
		{`{"version":1,"emoji":"❤️","emoji":"❤️"}`, ReactionMalformed},
		{`{"version":1,"emoji":"❤️","\u0065moji":"👍"}`, ReactionMalformed},
		{`{"version":1,"emoji":"❤️","extra":{"a":1,"a":2}}`, ReactionMalformed},
		{`{"version":1,"emoji":"❤️","extra":[{"a":1,"a":2}]}`, ReactionMalformed},
		{`{"version":1,"emoji":"❤️"} {}`, ReactionMalformed},
		{`{"version":1,"emoji":"❤️",}`, ReactionMalformed},
		{`[]`, ReactionMalformed}, {`null`, ReactionMalformed},
		{`{"version":1,"emoji":"\ud800"}`, ReactionMalformed},
		{`{"version":1,"emoji":"\ud83e\udeea"}`, ReactionMalformed},
	} {
		t.Run(tc.body, func(t *testing.T) {
			r := inspectReactions(reactionWithTarget(reactionPart(tc.body)))
			if r.Status != tc.status {
				t.Fatalf("status %q, want %q: %+v", r.Status, tc.status, r.Candidates)
			}
		})
	}
}

func TestReactionLayoutsAndBoundaries(t *testing.T) {
	attachment := validReaction()
	attachment.Headers = []*gmail.MessagePartHeader{{Name: "Content-Disposition", Value: `AtTaChMeNt; filename="r.json"`}}
	filename := validReaction()
	filename.Filename = "r.json"
	rfc := textPart("❤️ 👍🏽")
	rfc.Headers = []*gmail.MessagePartHeader{{Name: "Content-Disposition", Value: "reaction"}}
	conflicting := validReaction()
	conflicting.Headers = rfc.Headers
	for _, tc := range []struct {
		name, status, layout string
		p                    *gmail.MessagePart
		candidates           int
	}{
		{"alternative", ReactionStatusCandidate, "alternative", multipart("multipart/alternative", textPart("fallback"), validReaction(), htmlPart("<p>fallback</p>")), 1},
		{"mixed authored sibling", ReactionStatusCandidate, "other", multipart("multipart/mixed", textPart("authored prose"), validReaction()), 1},
		{"nested alternative", ReactionStatusCandidate, "other", multipart("multipart/mixed", multipart("multipart/alternative", textPart("fallback"), validReaction())), 1},
		{"duplicate plain alternative", ReactionStatusCandidate, "other", multipart("multipart/alternative", textPart("one"), validReaction(), textPart("two")), 1},
		{"duplicate markers", ReactionAmbiguous, "other", multipart("multipart/alternative", validReaction(), validReaction()), 2},
		{"contradictory markers", ReactionAmbiguous, "other", multipart("multipart/alternative", validReaction(), reactionPart(`{"version":1,"emoji":"👍"}`)), 2},
		{"attachment", ReactionNone, "other", attachment, 0},
		{"filename", ReactionNone, "other", filename, 0},
		{"attachment subtree", ReactionNone, "other", &gmail.MessagePart{MimeType: "multipart/mixed", Filename: "mail.mime", Parts: []*gmail.MessagePart{validReaction()}}, 0},
		{"embedded message", ReactionNone, "other", multipart("message/rfc822", validReaction()), 0},
		{"ordinary emoji", ReactionNone, "other", textPart("❤️"), 0},
		{"forged reaction prose", ReactionNone, "other", htmlPart(`<p>❤️</p><p>Somebody reacted via Gmail</p><blockquote>quoted message</blockquote>`), 0},
		{"localized prose", ReactionNone, "other", textPart("A réagi avec ❤️"), 0},
		{"RFC9078 unsupported", ReactionUnsupported, "other", rfc, 1},
		{"conflicting format markers", ReactionAmbiguous, "other", conflicting, 1},
		{"RFC and Gmail", ReactionAmbiguous, "other", multipart("multipart/alternative", rfc, validReaction()), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := inspectReactions(reactionWithTarget(tc.p))
			if r.Status != tc.status || r.Layout != tc.layout || len(r.Candidates) != tc.candidates {
				t.Fatalf("inspection %+v, want status=%s layout=%s candidates=%d", r, tc.status, tc.layout, tc.candidates)
			}
			if tc.name == "RFC9078 unsupported" && (r.Candidates[0].Body != "" || r.Candidates[0].BodyComplete) {
				t.Fatal("RFC body was consumed")
			}
		})
	}
}

func TestReactionInReplyToEvidence(t *testing.T) {
	for _, tc := range []struct {
		values []string
		status string
		target string
	}{
		{nil, ReactionIncomplete, ""},
		{[]string{"<target@example.test>"}, ReactionStatusCandidate, "<target@example.test>"},
		{[]string{" \r\n <target@example.test>\t"}, ReactionStatusCandidate, "<target@example.test>"},
		{[]string{"<one@example.test>", "<one@example.test>"}, ReactionAmbiguous, ""},
		{[]string{"<one@example.test>", "<two@example.test>"}, ReactionAmbiguous, ""},
		{[]string{"<one@example.test> <two@example.test>"}, ReactionAmbiguous, ""},
		{[]string{"(comment) <target@example.test>"}, ReactionAmbiguous, ""},
		{[]string{"not-a-message-id"}, ReactionAmbiguous, ""},
		{[]string{"<target@example.test>\r\nInjected: value"}, ReactionAmbiguous, ""},
	} {
		p := validReaction()
		for _, value := range tc.values {
			p.Headers = append(p.Headers, &gmail.MessagePartHeader{Name: "iN-rEpLy-To", Value: value})
		}
		p.Headers = append(p.Headers, &gmail.MessagePartHeader{Name: "References", Value: "<do-not-use@example.test>"})
		r := inspectReactions(p)
		if r.Status != tc.status || r.TargetMessageID != tc.target || !reflect.DeepEqual(r.InReplyTo, tc.values) {
			t.Errorf("headers %q: %+v", tc.values, r)
		}
	}
}

func TestReactionMIMEAndTransport(t *testing.T) {
	p := validReaction()
	p.MimeType = "Text/Vnd.Google.Email-Reaction+Json"
	p.Headers = []*gmail.MessagePartHeader{
		{Name: "Content-Type", Value: `Text/Vnd.Google.Email-Reaction+Json; Charset="UTF-8"`},
		{Name: "Content-Disposition", Value: `INLINE`},
	}
	p.Body.Data = base64.URLEncoding.EncodeToString([]byte(`{"version":1,"emoji":"👍🏽"}`))
	p.Body.Size = 0 // an omitted API size does not erase available bytes
	r := inspectReactions(reactionWithTarget(p))
	if r.Status != ReactionStatusCandidate || r.Candidates[0].Charset != "UTF-8" || r.Candidates[0].Disposition != "inline" || len(r.Candidates[0].ContentTypeHeaders) != 1 {
		t.Fatalf("parameterized case/padded transport: %+v", r)
	}
	for _, tc := range []struct {
		name, status string
		change       func(*gmail.MessagePart)
	}{
		{"nil body", ReactionIncomplete, func(p *gmail.MessagePart) { p.Body = nil }},
		{"empty data", ReactionIncomplete, func(p *gmail.MessagePart) { p.Body.Data = "" }},
		{"external data", ReactionIncomplete, func(p *gmail.MessagePart) { p.Body.AttachmentId = "external-body" }},
		{"bad base64", ReactionMalformed, func(p *gmail.MessagePart) { p.Body.Data = "$$$" }},
		{"bad UTF-8", ReactionMalformed, func(p *gmail.MessagePart) { p.Body.Data = base64.RawURLEncoding.EncodeToString([]byte{0xff}) }},
		{"size mismatch", ReactionIncomplete, func(p *gmail.MessagePart) { p.Body.Size = 900 }},
		{"oversize", ReactionIncomplete, func(p *gmail.MessagePart) { p.Body = partBody(strings.Repeat(" ", maxReactionPayloadBytes+1)) }},
		{"charset", ReactionUnsupported, func(p *gmail.MessagePart) { p.MimeType += "; charset=iso-8859-1" }},
		{"bad disposition", ReactionIncomplete, func(p *gmail.MessagePart) {
			p.Headers = []*gmail.MessagePartHeader{{Name: "Content-Disposition", Value: ";broken"}}
		}},
		{"repeated type", ReactionIncomplete, func(p *gmail.MessagePart) {
			p.Headers = []*gmail.MessagePartHeader{{Name: "Content-Type", Value: gmailReactionMIME}, {Name: "Content-Type", Value: gmailReactionMIME}}
		}},
		{"conflicting type", ReactionIncomplete, func(p *gmail.MessagePart) {
			p.Headers = []*gmail.MessagePartHeader{{Name: "Content-Type", Value: "text/plain"}}
		}},
		{"conflicting charset", ReactionIncomplete, func(p *gmail.MessagePart) {
			p.MimeType += "; charset=utf-8"
			p.Headers = []*gmail.MessagePartHeader{{Name: "Content-Type", Value: gmailReactionMIME + "; charset=iso-8859-1"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validReaction()
			tc.change(p)
			r := inspectReactions(reactionWithTarget(p))
			if r.Status != tc.status || r.TargetMessageID != "" {
				t.Fatalf("inspection %+v", r)
			}
		})
	}
}

func TestReactionInspectionBounds(t *testing.T) {
	t.Run("nil payload", func(t *testing.T) {
		if r := inspectReactions(nil); r.Status != ReactionIncomplete {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("depth", func(t *testing.T) {
		p := validReaction()
		for range maxReactionDepth + 1 {
			p = multipart("multipart/alternative", p)
		}
		if r := inspectReactions(reactionWithTarget(p)); r.Status != ReactionIncomplete || len(r.Candidates) != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("parts", func(t *testing.T) {
		p := multipart("multipart/mixed")
		for range maxReactionParts {
			p.Parts = append(p.Parts, textPart("body"))
		}
		p.Parts = append(p.Parts, validReaction())
		if r := inspectReactions(reactionWithTarget(p)); r.Status != ReactionIncomplete || len(r.Candidates) != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("decoded bytes", func(t *testing.T) {
		p := multipart("multipart/mixed")
		body := `{"version":1,"emoji":"❤️"}`
		body += strings.Repeat(" ", maxReactionPayloadBytes-len(body))
		for range 5 {
			p.Parts = append(p.Parts, reactionPart(body))
		}
		r := inspectReactions(reactionWithTarget(p))
		if r.Status != ReactionIncomplete || r.Candidates[4].BodyComplete {
			t.Fatalf("status=%s last=%+v", r.Status, r.Candidates[4])
		}
		bytes := 0
		for _, c := range r.Candidates {
			bytes += len(c.Body)
		}
		if bytes > maxReactionDecodedBytes {
			t.Fatal("decoded body budget exceeded")
		}
	})
	t.Run("header bytes", func(t *testing.T) {
		p := reactionWithTarget(validReaction())
		p.Headers[0].Value = strings.Repeat("x", maxReactionEvidenceBytes+1)
		r := inspectReactions(p)
		if r.Status != ReactionIncomplete || len(r.InReplyTo[0]) > maxReactionEvidenceBytes || r.TargetMessageID != "" {
			t.Fatalf("status=%s header bytes=%d", r.Status, len(r.InReplyTo[0]))
		}
	})
	t.Run("header repetitions", func(t *testing.T) {
		p := validReaction()
		for range 17 {
			reactionWithTarget(p)
		}
		r := inspectReactions(p)
		if r.Status != ReactionIncomplete || len(r.InReplyTo) != 16 {
			t.Fatalf("%+v", r)
		}
	})
}

func TestReactionReadThreadAndLegacyJSON(t *testing.T) {
	const plain = "❤️\nAn invented sender reacted via Gmail\n> invented quoted fallback"
	const markup = "<p>❤️</p><blockquote>invented quoted fallback</blockquote>"
	p := reactionWithTarget(multipart("multipart/alternative", textPart(plain), validReaction(), htmlPart(markup)))
	m := readReactionPayload(t, p, ReadOptions{IncludeHTML: true})
	if m.Body != plain || m.BodyHTML != markup || m.ReactionInspection.Status != ReactionStatusCandidate || m.ReactionInspection.Candidates[0].PartPath != "0.1" {
		t.Fatalf("Read changed fallback or lost evidence: %+v", m)
	}
	// The inspection is independent of display caps and whether HTML was requested.
	capped := readReactionPayload(t, p, ReadOptions{MaxBytes: 1})
	if capped.Body != "" || !capped.Truncated || !reflect.DeepEqual(capped.ReactionInspection, m.ReactionInspection) {
		t.Fatalf("body cap affected evidence: %+v", capped)
	}
	mixed := reactionWithTarget(multipart("multipart/mixed", p, inlineImagePart()))
	withAttachment := readReactionPayload(t, mixed, ReadOptions{IncludeHTML: true})
	if len(withAttachment.Attachments) != 1 || withAttachment.Body != plain || withAttachment.BodyHTML != markup || withAttachment.ReactionInspection.Layout != "other" {
		t.Fatalf("attachment/body regression: %+v", withAttachment)
	}
	external := reactionWithTarget(validReaction())
	external.Body = &gmail.MessagePartBody{AttachmentId: "synthetic-external", Size: 80}
	if got := readReactionPayload(t, external, ReadOptions{}).ReactionInspection; got.Status != ReactionIncomplete || got.Candidates[0].AttachmentID != "synthetic-external" {
		t.Fatalf("external body was not retained as incomplete: %+v", got)
	}
	containerBody := reactionWithTarget(multipart("multipart/alternative", validReaction(), textPart("fallback")))
	containerBody.Body = partBody("additional authored container data")
	if got := inspectReactions(containerBody); got.Layout != "other" {
		t.Fatalf("container body qualified as reaction-only: %+v", got)
	}
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.payloads["m1"] = p
	f.headers["m1"] = p.Headers
	f.threads["t-1"] = []string{"m1"}
	for _, full := range []bool{false, true} {
		thread, err := GetThread(context.Background(), f.service(t), emptyLabels(), "t-1", ReadOptions{IncludeHTML: full})
		if err != nil {
			t.Fatal(err)
		}
		got := thread.Messages[0].ReactionInspection
		if full && !reflect.DeepEqual(got, m.ReactionInspection) || !full && got != nil {
			t.Fatalf("full=%t inspection=%+v", full, got)
		}
		wire, _ := json.Marshal(thread)
		if strings.Contains(string(wire), `"reaction_inspection"`) != full {
			t.Fatalf("full=%t JSON: %s", full, wire)
		}
	}
	var legacy Message
	if err := json.Unmarshal([]byte(`{"id":"old","body":"❤️ reacted via Gmail"}`), &legacy); err != nil || legacy.ReactionInspection != nil {
		t.Fatalf("legacy metadata is not uninspected: %v %+v", err, legacy)
	}
	none := readReactionPayload(t, textPart("ordinary"), ReadOptions{})
	if none.ReactionInspection == nil || none.ReactionInspection.Status != ReactionNone {
		t.Fatal("inspected-none must differ from old decoder")
	}
	wire, _ := json.Marshal(m)
	var decoded Message
	if err := json.Unmarshal(wire, &decoded); err != nil || !reflect.DeepEqual(decoded.ReactionInspection, m.ReactionInspection) {
		t.Fatalf("Message wire lost evidence: %v", err)
	}
}
