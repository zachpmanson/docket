package mail

// Envelope is the lightweight shape returned by search/list — cheap fields
// only. Bodies are expensive and blow the context window, so the agent must
// call Read explicitly to get one. See docket-design.md §4.
type Envelope struct {
	ID         string   `json:"id"`
	ThreadID   string   `json:"thread_id"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Cc         string   `json:"cc,omitempty" verbose:"threading"`
	Subject    string   `json:"subject"`
	Date       string   `json:"date"`
	MessageID  string   `json:"message_id,omitempty" verbose:"threading"`
	InReplyTo  string   `json:"in_reply_to,omitempty" verbose:"threading"`
	References []string `json:"references,omitempty" verbose:"threading"`
	Labels     []string `json:"labels"`
	Snippet    string   `json:"snippet"`
}

// Attachment is metadata only; fetching bytes is a separate, explicit call
// (not yet implemented — phase 3 in the build order).
type Attachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
	PartID   string `json:"part_id"`
}

// html_status values, reported whenever the caller asked for HTML.
//
// A bool would not do. It reads false both when the message has no text/html
// part and when the caller never asked for one, and those demand opposite
// responses: accept the message as text-only, or fix the call. The field is
// absent when HTML was not requested, so all three states are distinct.
const (
	HTMLPresent = "present"
	HTMLNone    = "none"
)

// Message is the full shape returned by Read.
//
// BodyHTML is the text/html part exactly as the sender wrote it — no
// sanitising, no rewriting. Anything a renderer needs to strip it must strip
// itself, on the reasoning that a consumer can inspect its own sanitiser and
// cannot inspect ours.
//
// HTMLTruncated is reported separately from Truncated because the two bodies
// are capped independently and differ in size by an order of magnitude: an
// HTML part routinely exceeds a cap that the same message's text sits well
// under. One shared flag would tell a caller its text was cut when it was
// not, or its markup was whole when it was not. It appears only when true —
// exactly when the markup on hand is incomplete.
type Message struct {
	Envelope
	Body          string       `json:"body"`
	Truncated     bool         `json:"truncated"`
	HTMLStatus    string       `json:"html_status,omitempty"`
	BodyHTML      string       `json:"body_html,omitempty"`
	HTMLTruncated bool         `json:"html_truncated,omitempty"`
	Attachments   []Attachment `json:"attachments,omitempty"`
}

// ThreadMessage is one message inside a Thread: an envelope, plus the bodies
// when the caller asked for them. Every body field is omitted when empty, so
// a default thread read emits the same envelope-only objects it always has
// and a caller parsing them needs no change.
type ThreadMessage struct {
	Envelope
	Body          string `json:"body,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	HTMLStatus    string `json:"html_status,omitempty"`
	BodyHTML      string `json:"body_html,omitempty"`
	HTMLTruncated bool   `json:"html_truncated,omitempty"`
}

// Thread is the shape returned by the `thread` command: the thread id plus
// every message in it. Bodies are omitted unless asked for, because a
// conversation's worth of them is the single largest response docket can
// produce; without HTML the agent reads one message with Read instead.
type Thread struct {
	ThreadID string          `json:"thread_id"`
	Messages []ThreadMessage `json:"messages"`
}
