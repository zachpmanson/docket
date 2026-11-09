package mail

import (
	"encoding/base64"
	"fmt"
	"mime"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/api/gmail/v1"
)

const maxReactionFallbackBytes = 64 << 10

// ReactionFallback records completeness, not additional text to be displayed.
// Unavailable alternatives cannot vouch that suppressing the source is safe.
type ReactionFallback struct {
	PartPath     string `json:"part_path"`
	MIMEType     string `json:"mime_type"`
	Charset      string `json:"charset,omitempty"`
	DecodedBytes int    `json:"decoded_bytes"`
	BodyComplete bool   `json:"body_complete"`
}

func inspectReactionFallback(p *gmail.MessagePart, path, mediaType, charset string) ReactionFallback {
	f := ReactionFallback{PartPath: path, MIMEType: mediaType, Charset: charset}
	if charset != "" && !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "us-ascii") {
		return f
	}
	if p.Body == nil || p.Body.AttachmentId != "" || p.Body.Size < 0 ||
		p.Body.Size > maxReactionFallbackBytes || len(p.Body.Data) > base64.URLEncoding.EncodedLen(maxReactionFallbackBytes) {
		return f
	}
	encoding := base64.RawURLEncoding.Strict()
	if strings.HasSuffix(p.Body.Data, "=") {
		encoding = base64.URLEncoding.Strict()
	}
	b, err := encoding.DecodeString(p.Body.Data)
	if err != nil || !utf8.Valid(b) || len(b) > maxReactionFallbackBytes ||
		(p.Body.Size > 0 && p.Body.Size != int64(len(b))) {
		return f
	}
	if strings.EqualFold(charset, "us-ascii") {
		for _, c := range b {
			if c > 127 {
				return f
			}
		}
	}
	f.DecodedBytes, f.BodyComplete = len(b), true
	return f
}

// ValidateReactionInspection checks the versioned evidence contract before a
// downstream consumer classifies a notification. It reuses the MIME/payload and
// Unicode policy; it does not authenticate the sender or resolve account access.
// Missing, incomplete, unsupported or internally inconsistent evidence fails
// closed. Layout/completeness remain assertions from the MIME decoder, not a
// cryptographic attestation of an arbitrary serialized object.
func ValidateReactionInspection(r *ReactionInspection) error {
	if r == nil || r.Version != ReactionInspectionVersion || r.EmojiVersion != ReactionEmojiVersion ||
		r.Status != ReactionStatusCandidate || len(r.Candidates) != 1 || len(r.Reasons) != 0 ||
		len(r.InReplyTo) != 1 || len(r.InReplyTo[0]) > 2048 || !singleReactionTarget(strings.TrimSpace(r.InReplyTo[0])) ||
		r.TargetMessageID != strings.TrimSpace(r.InReplyTo[0]) {
		return fmt.Errorf("unsupported or ambiguous reaction inspection")
	}
	c := r.Candidates[0]
	if c.Format != GmailReactionFormat || c.Status != ReactionStatusCandidate || c.Reason != "" ||
		c.Version != 1 || !c.BodyComplete || c.AttachmentID != "" || len(c.Body) > maxReactionPayloadBytes ||
		c.MIMEType != gmailReactionMIME || len(c.PartID) > maxReactionEvidenceBytes ||
		len(c.Charset) > 128 || len(c.Disposition) > 128 || len(c.PartPath) > 16 ||
		len(c.ContentTypeHeaders) > 1 || len(c.DispositionHeaders) > 1 {
		return fmt.Errorf("unsupported or incomplete reaction candidate")
	}
	for _, headers := range [][]string{c.ContentTypeHeaders, c.DispositionHeaders} {
		for _, h := range headers {
			if len(h) > maxReactionEvidenceBytes {
				return fmt.Errorf("reaction header evidence too large")
			}
		}
	}
	if c.Disposition != "" && len(c.DispositionHeaders) == 0 {
		return fmt.Errorf("reaction disposition lacks source evidence")
	}
	mediaType := c.MIMEType
	if c.Charset != "" {
		mediaType = mime.FormatMediaType(mediaType, map[string]string{"charset": c.Charset})
	}
	p := &gmail.MessagePart{MimeType: mediaType, PartId: c.PartID,
		Body:    &gmail.MessagePartBody{Data: base64.RawURLEncoding.EncodeToString([]byte(c.Body)), Size: int64(len(c.Body))},
		Headers: []*gmail.MessagePartHeader{{Name: "In-Reply-To", Value: r.InReplyTo[0]}},
	}
	for _, h := range c.ContentTypeHeaders {
		p.Headers = append(p.Headers, &gmail.MessagePartHeader{Name: "Content-Type", Value: h})
	}
	for _, h := range c.DispositionHeaders {
		p.Headers = append(p.Headers, &gmail.MessagePartHeader{Name: "Content-Disposition", Value: h})
	}
	parsed := inspectReactions(p)
	if parsed.Status != ReactionStatusCandidate || parsed.Layout != "top-level" || len(parsed.Candidates) != 1 ||
		parsed.TargetMessageID != r.TargetMessageID || parsed.Candidates[0].Emoji != c.Emoji ||
		parsed.Candidates[0].Charset != c.Charset || parsed.Candidates[0].Disposition != c.Disposition {
		return fmt.Errorf("reaction payload and evidence disagree")
	}
	switch r.Layout {
	case "top-level":
		if c.PartPath != "0" || len(r.Fallbacks) != 0 {
			return fmt.Errorf("inconsistent top-level reaction layout")
		}
	case "alternative":
		if !directReactionPart(c.PartPath) || len(r.Fallbacks) > 2 {
			return fmt.Errorf("inconsistent reaction alternative layout")
		}
		seen := map[string]bool{c.PartPath: true}
		types := map[string]bool{}
		for _, f := range r.Fallbacks {
			if !directReactionPart(f.PartPath) || seen[f.PartPath] || types[f.MIMEType] ||
				(f.MIMEType != "text/plain" && f.MIMEType != "text/html") || !f.BodyComplete ||
				len(f.Charset) > 128 || len(f.PartPath) > 16 || f.DecodedBytes < 0 || f.DecodedBytes > maxReactionFallbackBytes ||
				(f.Charset != "" && !strings.EqualFold(f.Charset, "utf-8") && !strings.EqualFold(f.Charset, "us-ascii")) {
				return fmt.Errorf("incomplete or contradictory reaction fallback")
			}
			seen[f.PartPath], types[f.MIMEType] = true, true
		}
		for i := 0; i < len(seen); i++ {
			if !seen["0."+strconv.Itoa(i)] {
				return fmt.Errorf("reaction alternative has unaccounted parts")
			}
		}
	default:
		return fmt.Errorf("reaction layout does not establish reaction-only content")
	}
	return nil
}

func directReactionPart(path string) bool {
	if !strings.HasPrefix(path, "0.") {
		return false
	}
	n := strings.TrimPrefix(path, "0.")
	i, err := strconv.Atoi(n)
	return err == nil && i >= 0 && i < maxReactionParts && strconv.Itoa(i) == n
}
