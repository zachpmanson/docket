package mail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"regexp"
	"strings"
	"unicode/utf8"

	"google.golang.org/api/gmail/v1"
)

const (
	ReactionInspectionVersion = 2
	ReactionEmojiVersion      = "Unicode Emoji 16.0 (fully-qualified)"
	GmailReactionFormat       = "gmail-email-reaction/v1"
	gmailReactionMIME         = "text/vnd.google.email-reaction+json"

	ReactionNone            = "none"
	ReactionStatusCandidate = "candidate"
	ReactionUnsupported     = "unsupported"
	ReactionMalformed       = "malformed"
	ReactionIncomplete      = "incomplete"
	ReactionAmbiguous       = "ambiguous"

	maxReactionDepth         = 16
	maxReactionParts         = 128
	maxReactionPayloadBytes  = 4096
	maxReactionDecodedBytes  = 16384
	maxReactionEvidenceBytes = 32768
)

// ReactionInspection is structural, sender-supplied evidence, not an instruction
// to hide mail or proof of identity. A nil pointer means not inspected (including
// old decoders and metadata-only threads); Status=none means inspected, no marker.
// Only candidate with a TargetMessageID and a strict Layout is eligible for a
// downstream reaction-only policy; account/target resolution is NOT done here.
// See reactions.md for limits, status precedence and unsupported formats.
type ReactionInspection struct {
	Version         int                 `json:"version"`
	EmojiVersion    string              `json:"emoji_version"`
	Status          string              `json:"status"`
	Layout          string              `json:"layout"`                // top-level, alternative, or other
	InReplyTo       []string            `json:"in_reply_to,omitempty"` // complete repeated raw values unless incomplete
	TargetMessageID string              `json:"target_message_id,omitempty"`
	Candidates      []ReactionCandidate `json:"candidates,omitempty"`
	Fallbacks       []ReactionFallback  `json:"fallbacks,omitempty"`
	Reasons         []string            `json:"reasons,omitempty"`
}

// ReactionCandidate retains bounded original decoded JSON and MIME evidence.
// BodyComplete describes the bytes, independently of JSON validity. RFC9078
// markers are unsupported and their bodies are not decoded by this inspector.
type ReactionCandidate struct {
	Format             string   `json:"format"`
	PartPath           string   `json:"part_path"` // root is "0"; children are "0.0", "0.1", ...
	PartID             string   `json:"part_id,omitempty"`
	MIMEType           string   `json:"mime_type"`
	Charset            string   `json:"charset,omitempty"`
	Disposition        string   `json:"disposition,omitempty"`
	ContentTypeHeaders []string `json:"content_type_headers,omitempty"`
	DispositionHeaders []string `json:"disposition_headers,omitempty"`
	AttachmentID       string   `json:"attachment_id,omitempty"`
	Body               string   `json:"body,omitempty"`
	BodyComplete       bool     `json:"body_complete"`
	Version            int      `json:"version,omitempty"`
	Emoji              string   `json:"emoji,omitempty"`
	Status             string   `json:"status"`
	Reason             string   `json:"reason,omitempty"`
}

type reactionWalker struct {
	result     *ReactionInspection
	parts      int
	decoded    int
	evidence   int
	incomplete bool
	strict     bool
	rootType   string
	plain      int
	html       int
}

func inspectReactions(root *gmail.MessagePart) *ReactionInspection {
	r := &ReactionInspection{Version: ReactionInspectionVersion, EmojiVersion: ReactionEmojiVersion, Status: ReactionNone, Layout: "other"}
	w := reactionWalker{result: r, strict: true}
	if root != nil {
		r.InReplyTo = w.headers(root, "In-Reply-To")
	}
	w.walk(root, "0", 0)
	if w.strict && len(r.Candidates) == 1 && r.Candidates[0].Format == GmailReactionFormat {
		switch w.rootType {
		case gmailReactionMIME:
			r.Layout = "top-level"
		case "multipart/alternative":
			r.Layout = "alternative"
		}
	}
	for _, c := range r.Candidates {
		if c.Status == ReactionIncomplete {
			w.incomplete = true
		}
	}
	if w.incomplete {
		r.Status = ReactionIncomplete
	} else if len(r.Candidates) > 1 {
		r.Status = ReactionAmbiguous
		r.Reasons = append(r.Reasons, "multiple reaction markers (including identical duplicates)")
	} else if len(r.Candidates) == 1 {
		r.Status = r.Candidates[0].Status
	}
	if len(r.Candidates) != 0 {
		switch {
		case len(r.InReplyTo) == 0:
			if r.Status == ReactionStatusCandidate {
				r.Status = ReactionIncomplete
			}
			r.Reasons = append(r.Reasons, "missing In-Reply-To")
		case len(r.InReplyTo) != 1 || !singleReactionTarget(strings.TrimSpace(r.InReplyTo[0])):
			if !w.incomplete {
				r.Status = ReactionAmbiguous
			}
			r.Reasons = append(r.Reasons, "In-Reply-To is not one unambiguous bare Message-ID")
		case r.Status == ReactionStatusCandidate:
			r.TargetMessageID = strings.TrimSpace(r.InReplyTo[0])
		}
	}
	return r
}

// Conservative subset of msg-id: no comments, folding inside the ID, quoted
// local parts or non-ASCII. Preserve the raw header even when not supported.
var reactionTargetRE = regexp.MustCompile(`^<[^<>()[\]\\,;:"\s@]+@[^<>()[\]\\,;:"\s@]+>$`)

func singleReactionTarget(s string) bool {
	for _, b := range []byte(s) {
		if b < 33 || b > 126 {
			return false
		}
	}
	return reactionTargetRE.MatchString(s)
}

func (w *reactionWalker) fail(reason string) {
	if !w.incomplete { // one bounded reason suffices for the traversal failure
		w.result.Reasons = append(w.result.Reasons, reason)
	}
	w.incomplete = true
	w.strict = false
}

func (w *reactionWalker) retain(s string) string {
	remaining := maxReactionEvidenceBytes - w.evidence
	if len(s) > remaining {
		w.fail("MIME/header evidence limit exceeded; retained evidence is incomplete")
		s = truncateAtRune(s, remaining)
	}
	w.evidence += len(s)
	return s
}

func (w *reactionWalker) headers(p *gmail.MessagePart, name string) []string {
	var out []string
	for _, h := range p.Headers {
		if h != nil && strings.EqualFold(h.Name, name) {
			if len(out) == 16 {
				w.fail("repeated header limit exceeded")
				break
			}
			out = append(out, w.retain(h.Value))
		}
	}
	return out
}

func (w *reactionWalker) walk(p *gmail.MessagePart, path string, depth int) {
	if w.parts >= maxReactionParts || depth > maxReactionDepth {
		w.fail("MIME depth/part limit exceeded")
		return
	}
	w.parts++
	if p == nil {
		w.fail("missing MIME part")
		return
	}
	// Filename-bearing parts and MIME attachments are never outer-message
	// evidence, even if their children contain a syntactically valid marker.
	if p.Filename != "" {
		w.strict = false
		return
	}
	dh := w.headers(p, "Content-Disposition")
	disposition := ""
	if len(dh) > 0 {
		var err error
		disposition, _, err = mime.ParseMediaType(dh[0])
		if err != nil || len(dh) != 1 {
			w.fail("invalid or repeated Content-Disposition")
			return
		}
	}
	if disposition == "attachment" {
		w.strict = false
		return
	}
	cth := w.headers(p, "Content-Type")
	if len(p.MimeType) > 2048 {
		w.fail("MIME type byte limit exceeded")
		return
	}
	mt, params, err := mime.ParseMediaType(p.MimeType)
	if err != nil {
		w.fail("invalid MIME type")
		return
	}
	if len(cth) > 0 {
		hmt, hp, herr := mime.ParseMediaType(cth[0])
		if herr != nil || len(cth) != 1 || hmt != mt {
			w.fail("invalid, repeated or contradictory Content-Type")
			return
		}
		// Gmail normally exposes parameters only in the header. If both
		// representations include one, disagreement is not safe evidence.
		for k, v := range params {
			if hv, ok := hp[k]; ok && !strings.EqualFold(v, hv) {
				w.fail("contradictory MIME parameters")
				return
			}
		}
		for k, v := range hp {
			params[k] = v
		}
	}
	if depth == 0 {
		w.rootType = mt
	}
	if mt == "message/rfc822" || strings.HasPrefix(mt, "message/") {
		w.strict = false
		return
	}
	if disposition != "" && disposition != "inline" {
		w.strict = false
	}
	if mt == gmailReactionMIME || disposition == "reaction" {
		c := ReactionCandidate{
			Format: GmailReactionFormat, PartPath: path, PartID: w.retain(p.PartId),
			MIMEType: w.retain(mt), Charset: w.retain(params["charset"]), Disposition: w.retain(disposition),
			ContentTypeHeaders: cth, DispositionHeaders: dh,
		}
		switch {
		case disposition == "reaction":
			c.Format, c.Status, c.Reason = "rfc9078", ReactionUnsupported, "RFC9078 disposition is unsupported; body not consumed"
			if mt == gmailReactionMIME {
				c.Status, c.Reason = ReactionAmbiguous, "conflicting Gmail and RFC9078 markers"
			}
		case len(p.Parts) != 0:
			c.Status, c.Reason = ReactionMalformed, "reaction body has child parts"
		case c.Charset != "" && !strings.EqualFold(c.Charset, "utf-8"):
			c.Status, c.Reason = ReactionUnsupported, "unsupported reaction charset"
		default:
			w.payload(p, &c)
		}
		w.result.Candidates = append(w.result.Candidates, c)
		if depth > 1 || len(p.Parts) != 0 {
			w.strict = false
		}
		return
	}
	if mt == "multipart/alternative" && depth == 0 && len(p.Parts) > 0 {
		// Only direct, leaf alternatives qualify for the strict layout. An
		// unexpected container body could carry additional authored content.
		if p.Body != nil && (p.Body.Data != "" || p.Body.AttachmentId != "" || p.Body.Size != 0) {
			w.strict = false
		}
	} else if (mt == "text/plain" || mt == "text/html") && depth == 1 && len(p.Parts) == 0 {
		fallback := inspectReactionFallback(p, path, mt, params["charset"])
		w.result.Fallbacks = append(w.result.Fallbacks, fallback)
		if !fallback.BodyComplete {
			w.strict = false
		}
		if mt == "text/plain" {
			w.plain++
		} else {
			w.html++
		}
	} else {
		w.strict = false
	}
	if w.plain > 1 || w.html > 1 {
		w.strict = false
	}
	if !strings.HasPrefix(mt, "multipart/") && len(p.Parts) > 0 {
		w.fail("non-multipart body has child parts")
		return
	}
	for i, child := range p.Parts {
		if w.parts >= maxReactionParts {
			w.fail("MIME depth/part limit exceeded")
			break
		}
		w.walk(child, fmt.Sprintf("%s.%d", path, i), depth+1)
	}
}

func (w *reactionWalker) payload(p *gmail.MessagePart, c *ReactionCandidate) {
	c.Status, c.Reason = ReactionIncomplete, "missing or externally stored reaction body; no fetch attempted"
	if p.Body == nil {
		return
	}
	c.AttachmentID = w.retain(p.Body.AttachmentId)
	if p.Body.AttachmentId != "" || p.Body.Data == "" {
		return
	}
	coreLen := len(strings.TrimRight(p.Body.Data, "="))
	if len(p.Body.Data)-coreLen > 2 {
		c.Status, c.Reason = ReactionMalformed, "invalid base64url padding"
		return
	}
	decodedLen := base64.RawURLEncoding.DecodedLen(coreLen)
	if len(p.Body.Data) > base64.URLEncoding.EncodedLen(maxReactionPayloadBytes) ||
		decodedLen > maxReactionPayloadBytes || decodedLen > maxReactionDecodedBytes-w.decoded {
		c.Reason = "reaction payload byte limit exceeded"
		return
	}
	// Charge the conservative decoded-size estimate even for invalid transport,
	// so many malformed candidates cannot evade the aggregate decode budget.
	w.decoded += decodedLen
	encoding := base64.RawURLEncoding.Strict()
	if strings.HasSuffix(p.Body.Data, "=") {
		encoding = base64.URLEncoding.Strict()
	}
	b, err := encoding.DecodeString(p.Body.Data)
	if err != nil {
		c.Status, c.Reason = ReactionMalformed, "invalid base64url transport"
		return
	}
	if !utf8.Valid(b) {
		c.Status, c.Reason = ReactionMalformed, "reaction JSON is not UTF-8"
		return
	}
	c.Body = string(b)
	if p.Body.Size < 0 || (p.Body.Size > 0 && p.Body.Size != int64(len(b))) {
		c.Reason = "reaction body size mismatch"
		return
	}
	c.BodyComplete = true
	c.Status, c.Reason = parseReactionJSON(b, c)
}

func parseReactionJSON(b []byte, c *ReactionCandidate) (string, string) {
	// A token pass rejects duplicate keys at ALL nesting levels, including
	// unknown extension members, before the ordinary unmarshal loses them.
	d := json.NewDecoder(bytes.NewReader(b))
	if err := uniqueJSONValue(d); err != nil {
		return ReactionMalformed, "invalid JSON or duplicate keys"
	}
	if _, err := d.Token(); err != io.EOF {
		return ReactionMalformed, "trailing JSON data"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil || fields == nil {
		return ReactionMalformed, "reaction JSON must be an object"
	}
	v, ok := fields["version"]
	var version *int
	if !ok || json.Unmarshal(v, &version) != nil || version == nil {
		return ReactionMalformed, "missing or non-integer version"
	}
	c.Version = *version
	if c.Version != 1 {
		return ReactionUnsupported, "unsupported Gmail reaction version"
	}
	if json.Unmarshal(fields["emoji"], &c.Emoji) != nil || !reactionEmoji[c.Emoji] {
		c.Emoji = ""
		return ReactionMalformed, "not one supported Unicode emoji sequence"
	}
	return ReactionStatusCandidate, ""
}

func uniqueJSONValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return fmt.Errorf("duplicate or invalid key")
			}
			seen[s] = true
			if err := uniqueJSONValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSONValue(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = d.Token() // encoding/json validates the matching closing delimiter
	return err
}
