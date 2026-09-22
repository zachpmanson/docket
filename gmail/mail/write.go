package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"slices"
	"strings"

	"google.golang.org/api/gmail/v1"
)

// Body is what a message says, in the two forms it can be sent in. Text is the
// message and is required — it is the part every client can read, and the one a
// transcript shows. HTML is the same words marked up for a client that renders
// them, and empty means the message goes out as text/plain alone.
//
// Both are the caller's, in this package's usual division: something that knows
// what the message says decides how it is written, and docket decides how it is
// carried. Nothing here converts one into the other — a caller that wants both
// forms must build both, so the two parts of a single message can never
// disagree about what it says.
type Body struct {
	Text string
	HTML string
}

// SendPlan is a fully-resolved, ready-to-send message. Building one never
// mutates anything — it's safe to construct and show as a --dry-run
// preview before the caller decides whether to Execute it.
//
// Cc is empty for a reply to the sender alone and carries the rest of a
// message's audience for a reply-all (see PrepareReplyAll). Both fields are
// assembled here from the message being answered — and only from it: the
// recipients come from that message's own headers, minus the addresses of this
// mailbox, so there is no field a caller can name a recipient in. A plan can
// then be narrowed or rearranged within that audience before it is sent (see
// WithRecipients), which is what makes it a preview a reader can edit rather
// than a decision already made: the addresses it shows are still the addresses
// it uses, and no address the message did not carry can be added to it.
//
// Body is the plain-text part and HTML the alternative beside it, empty when the
// message is text alone (see buildRawMessage).
type SendPlan struct {
	To      string `json:"to"`
	Cc      string `json:"cc,omitempty"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	HTML    string `json:"html,omitempty"`

	// ToRecipients and CcRecipients are To and Cc as addresses rather than as the
	// header they will be written into: each with the display name the message
	// being answered gave it, in the order it carried them. The strings above are
	// these rendered, so the two always agree; these are what a caller rearranges
	// the audience from (see WithRecipients), and what a client shows as the
	// chips of a plan.
	ToRecipients []Recipient `json:"to_recipients,omitempty"`
	CcRecipients []Recipient `json:"cc_recipients,omitempty"`

	raw      string
	threadID string
	// opts is the whole message this plan was built from, kept so that a plan
	// rebuilt against a different audience is the same message with a different
	// To/Cc rather than a patch of bytes that were already serialised (see
	// WithRecipients).
	opts rawMessageOptions
}

// Recipient is one address a plan carries, with the display name the message
// being answered gave it. The Address is what a message is sent to; the Name is
// what a reader is shown it as. A plan's recipients are the whole of the
// audience it may use: an address outside this set is refused (see
// WithRecipients), so a reply can reach only what the message it answers already
// carried.
type Recipient struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// PrepareSend validates a send request and builds the raw RFC 5322 message,
// without sending anything.
func PrepareSend(to, subject string, body Body) (*SendPlan, error) {
	if _, err := mail.ParseAddressList(to); err != nil {
		return nil, fmt.Errorf(
			"--to %q is not a valid address list: %w (expected e.g. \"a@example.com\" "+
				"or \"a@example.com, b@example.com\")", to, err)
	}
	opts := rawMessageOptions{To: to, Subject: subject, Body: body.Text, HTML: body.HTML}
	raw, err := buildRawMessage(opts)
	if err != nil {
		return nil, err
	}
	sent, err := addresses(to, "To")
	if err != nil {
		return nil, err
	}
	return &SendPlan{
		To: to, Subject: subject, Body: body.Text, HTML: body.HTML,
		ToRecipients: recipientsOf(sent), raw: raw, opts: opts,
	}, nil
}

// WithRecipients is this plan with a chosen audience: the addresses it is to
// carry in to and in cc, each named as one of the plan's own recipients (see
// ToRecipients/CcRecipients). The raw message is built again from the plan's own
// fields rather than patched, so what a caller sends is what the plan now holds.
//
// It may only rearrange what the plan already carries, never widen it. An address
// that is not one of the plan's recipients is refused — and because those were
// resolved from the answered message's headers minus this mailbox's own, an
// address the message did not carry has no spelling that reaches it here. An
// empty to is refused too: a message with nobody on it is not a narrower version
// of this one. An address may appear once and in one list, since a recipient is
// one recipient.
//
// A plan is not mutated: the returned plan is a copy, and calling this twice from
// the same plan starts from the same audience.
func (p *SendPlan) WithRecipients(to, cc []string) (*SendPlan, error) {
	known := map[string]Recipient{}
	for _, list := range [][]Recipient{p.ToRecipients, p.CcRecipients} {
		for _, r := range list {
			key := strings.ToLower(r.Address)
			if _, ok := known[key]; !ok {
				known[key] = r
			}
		}
	}
	pick := func(list []string, what string) ([]Recipient, error) {
		out := make([]Recipient, 0, len(list))
		seen := map[string]bool{}
		for _, want := range list {
			addr, err := mail.ParseAddress(strings.TrimSpace(want))
			if err != nil {
				return nil, fmt.Errorf("the %s address %q is not an address: %w", what, want, err)
			}
			key := addressKey(addr)
			if seen[key] {
				return nil, fmt.Errorf("the %s list names %q twice", what, want)
			}
			seen[key] = true
			r, ok := known[key]
			if !ok {
				return nil, fmt.Errorf(
					"%q is not an address the message being answered carried, so the reply "+
						"cannot reach it: the audience is the message's own, and this can only "+
						"take people off it or move them between To and Cc", want)
			}
			out = append(out, r)
		}
		return out, nil
	}

	toList, err := pick(to, "to")
	if err != nil {
		return nil, err
	}
	ccList, err := pick(cc, "cc")
	if err != nil {
		return nil, err
	}
	if len(toList) == 0 {
		return nil, errors.New("a reply needs somebody in To: to names no address")
	}
	inTo := map[string]bool{}
	for _, r := range toList {
		inTo[strings.ToLower(r.Address)] = true
	}
	for _, r := range ccList {
		if inTo[strings.ToLower(r.Address)] {
			return nil, fmt.Errorf(
				"%q is in both To and Cc: one recipient is one address, in one list", r.Address)
		}
	}

	toStr, ccStr := joinRecipients(toList), joinRecipients(ccList)
	opts := p.opts
	opts.To, opts.Cc = toStr, ccStr
	raw, err := buildRawMessage(opts)
	if err != nil {
		return nil, err
	}
	out := *p
	out.To, out.Cc = toStr, ccStr
	out.ToRecipients, out.CcRecipients = toList, ccList
	out.raw, out.opts = raw, opts
	return &out, nil
}

// recipientsOf is a parsed address list as a plan holds it, keeping the names
// the message gave each address.
func recipientsOf(list []*mail.Address) []Recipient {
	out := make([]Recipient, 0, len(list))
	for _, a := range list {
		out = append(out, Recipient{Name: a.Name, Address: a.Address})
	}
	return out
}

// joinRecipients renders chosen recipients the way a header carries them, through
// the same formatAddress a plan's own To and Cc are built with.
func joinRecipients(list []Recipient) string {
	addrs := make([]*mail.Address, 0, len(list))
	for _, r := range list {
		addrs = append(addrs, &mail.Address{Name: r.Name, Address: r.Address})
	}
	return joinAddresses(addrs)
}

// PrepareReply fetches the message being replied to (a read-only call) and
// builds a threaded raw message (In-Reply-To/References + Gmail's
// ThreadId), without sending anything. It answers the sender alone; see
// PrepareReplyAll for everyone the message reached.
func PrepareReply(ctx context.Context, svc *gmail.Service, id string, body Body) (*SendPlan, error) {
	return prepareReply(ctx, svc, id, body, false)
}

// PrepareReplyAll is PrepareReply to everyone the message was addressed to: the
// sender in To, and the rest of the audience — the original To and Cc — in Cc,
// minus the addresses of the mailbox doing the replying.
//
// It is the same two reads PrepareReply makes plus two more (see ownAddresses),
// and it exists because "reply" and "reply all" are one decision the mailbox can
// make and a caller cannot: which of the addresses on a message are the ones
// reading it. A caller assembling this itself would be asking a second question
// (who am I?) that only the account can answer.
//
// Taking the reader off the audience is a rule about *their* addresses, not a rule
// that a reply cannot be to one of them: when every address on the message is the
// mailbox's own, the sender comes back on the reply and it answers the reader (see
// replyRecipients). A reply always has somewhere to go.
func PrepareReplyAll(ctx context.Context, svc *gmail.Service, id string, body Body) (*SendPlan, error) {
	return prepareReply(ctx, svc, id, body, true)
}

// prepareReply is the one path both replies take, so reply-all cannot drift
// from reply in threading, subject, or body: the only difference is who is on
// the message.
func prepareReply(ctx context.Context, svc *gmail.Service, id string, body Body, all bool) (*SendPlan, error) {
	original, err := svc.Users.Messages.Get(meUser, id).
		Format("metadata").
		MetadataHeaders("Message-Id", "References", "Subject", "From", "Reply-To", "To", "Cc").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf(
			"fetching original message %q to reply to: %w (ids come from `mail search`/"+
				"`mail list`/`mail thread` output)", id, err)
	}

	h := original.Payload.Headers
	messageID := header(h, "Message-Id")
	references := header(h, "References")
	if references == "" {
		references = messageID
	} else if messageID != "" {
		references = references + " " + messageID
	}
	subject := header(h, "Subject")
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}

	to, cc, err := replyRecipients(ctx, svc, h, all)
	if err != nil {
		return nil, fmt.Errorf("assembling the reply to %q: %w", id, err)
	}

	toStr, ccStr := joinAddresses(to), joinAddresses(cc)
	opts := rawMessageOptions{
		To: toStr, Cc: ccStr, Subject: subject, Body: body.Text, HTML: body.HTML,
		InReplyTo: messageID, References: references,
	}
	raw, err := buildRawMessage(opts)
	if err != nil {
		return nil, err
	}

	return &SendPlan{
		To: toStr, Cc: ccStr, Subject: subject, Body: body.Text, HTML: body.HTML,
		ToRecipients: recipientsOf(to), CcRecipients: recipientsOf(cc),
		raw: raw, threadID: original.ThreadId, opts: opts,
	}, nil
}

// replyRecipients assembles the To (and, for a reply-all, Cc) of a reply from
// the headers of the message being answered.
//
// The sender of the reply is the original message's Reply-To when it has one and
// its From otherwise: a Reply-To is a request to answer elsewhere — a list
// posting to its members — and answering the From instead would send the reply
// where it was asked not to go. Every address is re-parsed and re-serialised
// through net/mail rather than copied across as header text, so a display name
// keeps its name and a header cannot smuggle a second one (see buildRawMessage).
//
// For a reply-all the rest of the audience is the original To and then its Cc,
// in that order, minus the addresses of this mailbox and minus anyone already in
// To: an address appearing in both lists is one recipient, and one that appears
// only in To does not reappear in Cc. If removing this mailbox from the headers
// leaves To empty — answering a message you sent yourself — the first of the
// remaining recipients is promoted into To, because a message needs one and
// replying to yourself is not the alternative.
func replyRecipients(ctx context.Context, svc *gmail.Service, h []*gmail.MessagePartHeader, all bool) (to, cc []*mail.Address, err error) {
	sender, err := addresses(header(h, "Reply-To"), "Reply-To")
	if err != nil {
		return nil, nil, err
	}
	if len(sender) == 0 {
		sender, err = addresses(header(h, "From"), "From")
		if err != nil {
			return nil, nil, err
		}
	}
	if len(sender) == 0 {
		return nil, nil, fmt.Errorf("the message has no From header to reply to")
	}
	if !all {
		return sender, nil, nil
	}

	own, err := ownAddresses(ctx, svc)
	if err != nil {
		return nil, nil, err
	}
	others, err := addresses(header(h, "To"), "To")
	if err != nil {
		return nil, nil, err
	}
	more, err := addresses(header(h, "Cc"), "Cc")
	if err != nil {
		return nil, nil, err
	}
	others = append(others, more...)

	var toList []*mail.Address
	for _, a := range sender {
		if !own[addressKey(a)] {
			toList = append(toList, a)
		}
	}
	var ccList []*mail.Address
	seen := map[string]bool{}
	for _, a := range toList {
		seen[addressKey(a)] = true
	}
	for _, a := range others {
		key := addressKey(a)
		if own[key] || seen[key] {
			continue
		}
		seen[key] = true
		// Into Cc, unless To is empty — then the first one goes there.
		if len(toList) == 0 {
			toList = append(toList, a)
			continue
		}
		ccList = append(ccList, a)
	}
	// A message whose whole audience is this mailbox — a note to self, or two of
	// the reader's own addresses talking to each other — has nobody left once the
	// reader is taken off it, and a message needs a recipient. So the sender comes
	// back: answering yourself is what a reply to such a message means, and it is
	// the only reader this package can name without inventing an address. The
	// alternative (refusing) tells a reader who wants to add a line to their own
	// thread that they cannot reply to it, which is both true of no other mail
	// client and a dead end: there is no field here to name somebody else.
	//
	// Note what this does *not* do: the rest of the audience stays off. When the
	// message was one of the reader's addresses writing to another, the reply goes
	// to the one that wrote, not to every alias the reader owns.
	if len(toList) == 0 {
		toList = sender
	}
	return toList, ccList, nil
}

// addressKey is how two addresses are compared: the address without its display
// name, case-folded. The domain is case-insensitive by DNS and the local part is
// case-insensitive in practice everywhere this runs, so a mailbox that answers
// to Ann@example.com is the same mailbox as ann@example.com — and one address
// appearing in both To and Cc is one recipient, not two.
//
// What is NOT folded is what goes into the message: the address is written as the
// message being answered had it, because which spelling is canonical is not
// something this package knows.
func addressKey(a *mail.Address) string {
	return strings.ToLower(a.Address)
}

// ownAddresses is every address this mailbox answers to: the account's own
// address and each of its send-as aliases, lowercased for comparison.
//
// This is the question a reply-all must not guess at — the aliases are the whole
// reason to ask, since the mail a reader answers is usually addressed to one of
// them and not to the account's own name — and only the mailbox can answer it.
// Both reads are inside the grant the rest of this package already runs on, so
// nothing new is consented to (users.getProfile and users.settings.sendAs.list
// each accept https://mail.google.com/). A caller cannot supply the set instead:
// a reply-all assembled from a guess about who the reader is would CC them on
// their own correspondence, or drop a recipient it mistook for them.
func ownAddresses(ctx context.Context, svc *gmail.Service) (map[string]bool, error) {
	profile, err := svc.Users.GetProfile(meUser).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("asking the mailbox which address is its own: %w", err)
	}
	own := map[string]bool{}
	if profile.EmailAddress != "" {
		own[strings.ToLower(profile.EmailAddress)] = true
	}
	// Every send-as alias, verified or not: the question here is which addresses
	// are this account's, and an alias the mailbox lists is one of them whatever
	// its verification state.
	aliases, err := svc.Users.Settings.SendAs.List(meUser).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("asking the mailbox for its send-as aliases: %w", err)
	}
	for _, a := range aliases.SendAs {
		if a.SendAsEmail != "" {
			own[strings.ToLower(a.SendAsEmail)] = true
		}
	}
	return own, nil
}

// addresses parses one header's address list. An absent header has no
// addresses; a header that is present and unparsable is an error rather than an
// empty list, because a reply that quietly reaches fewer people than the message
// did is worse than one that does not go out. `what` names the header in that
// error, since the message is the caller's to look at.
func addresses(value, what string) ([]*mail.Address, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	list, err := mail.ParseAddressList(value)
	if err != nil {
		return nil, fmt.Errorf("the message's %s header %q is not an address list: %w", what, value, err)
	}
	return list, nil
}

// joinAddresses renders a list the way a header carries it, each address as
// formatAddress prints one. Blank entries are dropped rather than yielding a
// trailing comma in a header.
func joinAddresses(list []*mail.Address) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		parts = append(parts, formatAddress(a))
	}
	return strings.Join(parts, ", ")
}

// formatAddress renders one recipient as a header carries it.
//
// Go's Address.String quotes every display name, which is correct and would make
// every plan read as "Dana Okafor" <dana@example.com> — punctuation in the middle
// of a sentence a person is checking before they send it. So a name that needs no
// quoting gets none, and anything else — a comma, a colon, a letter outside ASCII
// — goes to Go's own serialiser, which quotes or encodes it properly.
//
// Either way the text is built from a parsed address rather than copied out of
// somebody else's header: what a plan shows is something this package wrote.
func formatAddress(a *mail.Address) string {
	if !cleanAddrSpec(a.Address) {
		return a.String()
	}
	if a.Name == "" {
		return a.Address
	}
	if !cleanName(a.Name) {
		return a.String()
	}
	return a.Name + " <" + a.Address + ">"
}

// cleanName reports whether a display name is the shape that needs no quoting:
// letters, digits, spaces and the handful of marks that appear in names. Anything
// exotic is left to net/mail, which knows what to do with it.
func cleanName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ', r == '.', r == '_', r == '+', r == '-', r == '\'':
		default:
			return false
		}
	}
	return true
}

// cleanAddrSpec reports whether an address is local@domain with neither half
// needing quotes: the local part the atext characters `net/mail` accepts, and a
// domain of letters, digits, dots and hyphens. The check is what lets an address
// be written as it was parsed rather than through Go's quoting path.
func cleanAddrSpec(addr string) bool {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return false
	}
	for _, r := range addr[:at] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-/=?^_`{|}~.", r):
		default:
			return false
		}
	}
	for _, r := range addr[at+1:] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// Execute sends a prepared message.
func (p *SendPlan) Execute(ctx context.Context, svc *gmail.Service, labels *LabelCache) (*Envelope, error) {
	sent, err := svc.Users.Messages.Send(meUser, &gmail.Message{
		Raw:      p.raw,
		ThreadId: p.threadID,
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sending message: %w", err)
	}
	return fetchEnvelope(ctx, svc, labels, sent.Id)
}

// LabelPlan is a fully-resolved label add/remove request.
type LabelPlan struct {
	MessageID    string   `json:"message_id"`
	AddLabels    []string `json:"add_labels,omitempty"`
	RemoveLabels []string `json:"remove_labels,omitempty"`

	addIDs, removeIDs []string
}

// PrepareLabel resolves label names to ids (read-only — LabelCache is
// already loaded) without modifying anything.
func PrepareLabel(labels *LabelCache, messageID string, addNames, removeNames []string) (*LabelPlan, error) {
	addIDs, err := resolveLabelNames(labels, addNames)
	if err != nil {
		return nil, err
	}
	removeIDs, err := resolveLabelNames(labels, removeNames)
	if err != nil {
		return nil, err
	}
	return &LabelPlan{
		MessageID: messageID, AddLabels: addNames, RemoveLabels: removeNames,
		addIDs: addIDs, removeIDs: removeIDs,
	}, nil
}

func resolveLabelNames(labels *LabelCache, names []string) ([]string, error) {
	ids := make([]string, 0, len(names))
	for _, name := range names {
		id, ok := labels.ID(name)
		if !ok {
			return nil, fmt.Errorf("no label named %q; known labels: %v", name, labels.AllNames())
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Execute applies a prepared label change.
func (p *LabelPlan) Execute(ctx context.Context, svc *gmail.Service, labels *LabelCache) (*Envelope, error) {
	_, err := svc.Users.Messages.Modify(meUser, p.MessageID, &gmail.ModifyMessageRequest{
		AddLabelIds:    p.addIDs,
		RemoveLabelIds: p.removeIDs,
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf(
			"modifying labels on message %q: %w (ids come from `mail search`/`mail list` output)",
			p.MessageID, err)
	}
	return fetchEnvelope(ctx, svc, labels, p.MessageID)
}

func fetchEnvelope(ctx context.Context, svc *gmail.Service, labels *LabelCache, id string) (*Envelope, error) {
	msg, err := svc.Users.Messages.Get(meUser, id).
		Format("metadata").
		MetadataHeaders(metadataHeaders...).
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("fetching message %q after write: %w", id, err)
	}
	env := envelopeFromMessage(msg, labels)
	return &env, nil
}

type rawMessageOptions struct {
	To, Cc, Subject, Body string
	HTML                  string
	InReplyTo, References string
}

// buildRawMessage serialises the message. Every header value it is handed has
// already been assembled from parsed addresses (see replyRecipients) or encoded
// (the subject), and it refuses one that still carries a line break: a header
// built from somebody else's header is the one place a newline could become a
// new header, and "no CR or LF survives into this message" is a property worth
// holding here rather than inheriting from a quoter somewhere else.
//
// With no HTML the message is one text/plain part, exactly as it has always been.
// With it the message is multipart/alternative: the same message twice, plain
// first and HTML second, which is the order the format requires — the parts run
// from the plainest to the richest, and a client that understands both takes the
// last one it can read. The two are written as they were handed in; nothing here
// converts one into the other, so the parts cannot say different things.
func buildRawMessage(o rawMessageOptions) (string, error) {
	var buf bytes.Buffer
	set := func(name, value string) error {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("refusing to write a %s header containing a line break: %q", name, value)
		}
		fmt.Fprintf(&buf, "%s: %s\r\n", name, value)
		return nil
	}
	if err := set("To", o.To); err != nil {
		return "", err
	}
	if o.Cc != "" {
		if err := set("Cc", o.Cc); err != nil {
			return "", err
		}
	}
	if err := set("Subject", mime.QEncoding.Encode("UTF-8", o.Subject)); err != nil {
		return "", err
	}
	if o.InReplyTo != "" {
		if err := set("In-Reply-To", o.InReplyTo); err != nil {
			return "", err
		}
	}
	if o.References != "" {
		if err := set("References", o.References); err != nil {
			return "", err
		}
	}
	buf.WriteString("MIME-Version: 1.0\r\n")

	if o.HTML == "" {
		buf.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
		buf.WriteString("\r\n")
		buf.WriteString(o.Body)
		return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(buf.Bytes()), nil
	}

	boundary, err := messageBoundary(o.Body, o.HTML)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%q\r\n", boundary)
	buf.WriteString("\r\n")
	// The delimiter goes at the start of its own line, so each part is separated
	// from the next by a CRLF that belongs to the framing rather than to the text —
	// a body already ending in one would otherwise leave a blank line inside the
	// part that nobody wrote.
	for _, part := range []struct{ kind, text string }{
		{"text/plain", o.Body},
		{"text/html", o.HTML},
	} {
		fmt.Fprintf(&buf, "--%s\r\n", boundary)
		fmt.Fprintf(&buf, "Content-Type: %s; charset=\"UTF-8\"\r\n", part.kind)
		buf.WriteString("\r\n")
		buf.WriteString(strings.TrimRight(part.text, "\r\n"))
		buf.WriteString("\r\n")
	}
	fmt.Fprintf(&buf, "--%s--\r\n", boundary)

	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(buf.Bytes()), nil
}

// messageBoundary picks the token that separates the parts, and it is the one
// place a multipart message can be broken by its own content: a boundary that
// occurs inside a part ends that part early. There is no token that cannot
// occur in text somebody wrote, so the one chosen is the first of a numbered
// family that does not occur in either part as handed in.
func messageBoundary(parts ...string) (string, error) {
	for i := 0; i < 100; i++ {
		candidate := fmt.Sprintf("=_docket_%d_=", i)
		if !slices.ContainsFunc(parts, func(p string) bool { return strings.Contains(p, candidate) }) {
			return candidate, nil
		}
	}
	return "", errors.New(
		"could not find a MIME boundary that does not occur in the message: " +
			"every candidate this builds is inside the body")
}
