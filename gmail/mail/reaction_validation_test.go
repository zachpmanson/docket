package mail

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
)

func TestReactionIncompleteFallbackCannotVouchForLayout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*gmail.MessagePart)
	}{
		{"missing", func(p *gmail.MessagePart) { p.Body = nil }},
		{"external", func(p *gmail.MessagePart) { p.Body.AttachmentId = "synthetic-external" }},
		{"wrong size", func(p *gmail.MessagePart) { p.Body.Size += 5 }},
		{"bad transport", func(p *gmail.MessagePart) { p.Body.Data = "!!!" }},
		{"oversized", func(p *gmail.MessagePart) { p.Body.Size = maxReactionFallbackBytes + 1 }},
		{"unsupported charset", func(p *gmail.MessagePart) { p.MimeType = "text/plain; charset=x-unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallback := textPart("Synthetic original message")
			tc.change(fallback)
			r := inspectReactions(reactionWithTarget(multipart("multipart/alternative", fallback, validReaction())))
			if r.Status != ReactionStatusCandidate || r.Layout != "other" || len(r.Fallbacks) != 1 || r.Fallbacks[0].BodyComplete {
				t.Fatalf("unsafe fallback layout: %+v", r)
			}
			if ValidateReactionInspection(r) == nil {
				t.Fatal("incomplete fallback was eligible for suppression")
			}
		})
	}
}

func TestValidateReactionInspectionRoundTrip(t *testing.T) {
	for _, root := range []*gmail.MessagePart{
		reactionWithTarget(validReaction()),
		reactionWithTarget(multipart("multipart/alternative", textPart("fallback"), validReaction(), htmlPart("<p>fallback</p>"))),
	} {
		r := inspectReactions(root)
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ReactionInspection
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := ValidateReactionInspection(&decoded); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateReactionInspectionRefusesContradictions(t *testing.T) {
	base := inspectReactions(reactionWithTarget(multipart("multipart/alternative", textPart("fallback"), validReaction())))
	for _, tc := range []struct {
		name   string
		change func(*ReactionInspection)
	}{
		{"version", func(r *ReactionInspection) { r.Version++ }},
		{"none", func(r *ReactionInspection) { r.Status = ReactionNone }},
		{"emoji version", func(r *ReactionInspection) { r.EmojiVersion = "unknown" }},
		{"target", func(r *ReactionInspection) { r.TargetMessageID = "<other@example.test>" }},
		{"duplicate target", func(r *ReactionInspection) { r.InReplyTo = append(r.InReplyTo, r.InReplyTo[0]) }},
		{"body emoji", func(r *ReactionInspection) { r.Candidates[0].Emoji = "👍" }},
		{"body version", func(r *ReactionInspection) { r.Candidates[0].Body = `{"version":2,"emoji":"❤️"}` }},
		{"duplicate JSON key", func(r *ReactionInspection) { r.Candidates[0].Body = `{"version":1,"version":1,"emoji":"❤️"}` }},
		{"nonemoji", func(r *ReactionInspection) { r.Candidates[0].Body = `{"version":1,"emoji":"A"}` }},
		{"candidate incomplete", func(r *ReactionInspection) { r.Candidates[0].BodyComplete = false }},
		{"attachment", func(r *ReactionInspection) { r.Candidates[0].Disposition = "attachment" }},
		{"nested candidate", func(r *ReactionInspection) { r.Candidates[0].PartPath = "0.0.0" }},
		{"false top level", func(r *ReactionInspection) { r.Layout = "top-level" }},
		{"fallback incomplete", func(r *ReactionInspection) { r.Fallbacks[0].BodyComplete = false }},
		{"fallback candidate collision", func(r *ReactionInspection) { r.Fallbacks[0].PartPath = r.Candidates[0].PartPath }},
		{"payload oversized", func(r *ReactionInspection) { r.Candidates[0].Body = strings.Repeat(" ", maxReactionPayloadBytes+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(base)
			var r ReactionInspection
			if err := json.Unmarshal(b, &r); err != nil {
				t.Fatal(err)
			}
			tc.change(&r)
			if err := ValidateReactionInspection(&r); err == nil {
				t.Fatal("contradictory inspection accepted")
			}
		})
	}
	if ValidateReactionInspection(nil) == nil {
		t.Fatal("old decoder absence accepted")
	}
}
