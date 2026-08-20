package mail

// Envelope is the lightweight shape returned by search/list — cheap fields
// only. Bodies are expensive and blow the context window, so the agent must
// call Read explicitly to get one. See docket-design.md §4.
type Envelope struct {
	ID         string   `json:"id"`
	ThreadID   string   `json:"thread_id"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Cc         string   `json:"cc,omitempty"`
	Subject    string   `json:"subject"`
	Date       string   `json:"date"`
	MessageID  string   `json:"message_id,omitempty"`
	InReplyTo  string   `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`
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

// Message is the full shape returned by Read.
type Message struct {
	Envelope
	Body        string       `json:"body"`
	Truncated   bool         `json:"truncated"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Thread is the shape returned by the `thread` command: the thread id plus
// every message in it, envelope-only (agent reads a specific message with
// Read if it needs the body).
type Thread struct {
	ThreadID string     `json:"thread_id"`
	Messages []Envelope `json:"messages"`
}
