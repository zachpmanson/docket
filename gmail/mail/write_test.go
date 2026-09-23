package mail

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
)

// The write path's plans: who a reply is addressed to, what the raw message
// holds, and the two reads a reply-all needs to know which addresses are the
// mailbox's own.
//
// Everything here is invented. The mailbox is the fake in search_test.go, driven
// through its endpoint, so nothing in this file touches a network or a real
// account.

// replyFixture builds a fake holding one message with the given headers, and the
// service a plan is built over. The headers are the whole input to the recipient
// assembly, so each test states its own.
func replyFixture(t *testing.T, h ...*gmail.MessagePartHeader) *gmail.Service {
	t.Helper()
	f := newFakeGmail(t, map[string]listPage{})
	f.bodies["m1"] = "the message being answered"
	f.headers["m1"] = append(h, &gmail.MessagePartHeader{
		Name: "Message-ID", Value: "<m1@mail.example.com>",
	})
	return f.service(t)
}

func hdr(name, value string) *gmail.MessagePartHeader {
	return &gmail.MessagePartHeader{Name: name, Value: value}
}

// raw is the message the plan would put on the wire, decoded: the only way to
// assert what was built rather than what was summarised.
func raw(t *testing.T, p *SendPlan) string {
	t.Helper()
	decoded, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(p.raw)
	if err != nil {
		t.Fatalf("the plan's raw message is not base64url: %v", err)
	}
	return string(decoded)
}

func TestReplyAnswersTheSenderAlone(t *testing.T) {
	svc := replyFixture(t,
		hdr("From", "Dana Okafor <dana@example.com>"),
		hdr("To", "reader@example.com, carl@example.net"),
		hdr("Cc", "carl@example.net"),
		hdr("Subject", "quarterly widget audit"),
	)
	plan, err := PrepareReply(context.Background(), svc, "m1", Body{Text: "thanks"})
	if err != nil {
		t.Fatalf("preparing the reply: %v", err)
	}
	// The sender and nobody else: the addresses on the message are the reason
	// reply-all exists as a separate call.
	if plan.To != "Dana Okafor <dana@example.com>" {
		t.Errorf("To = %q, want the sender alone", plan.To)
	}
	if plan.Cc != "" {
		t.Errorf("Cc = %q, want empty", plan.Cc)
	}
	if plan.Subject != "Re: quarterly widget audit" {
		t.Errorf("Subject = %q", plan.Subject)
	}
	got := raw(t, plan)
	if strings.Contains(got, "Cc:") {
		t.Errorf("a reply to the sender carries a Cc header:\n%s", got)
	}
	for _, want := range []string{
		"To: Dana Okafor <dana@example.com>\r\n",
		"In-Reply-To: <m1@mail.example.com>\r\n",
		"References: <m1@mail.example.com>\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("raw message is missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "thanks") {
		t.Errorf("the body is not the end of the message:\n%s", got)
	}
}

func TestReplyAnswersAReplyToRatherThanTheSender(t *testing.T) {
	// A list posting to its members: answering the From would send the reply
	// where the message asked it not to go.
	svc := replyFixture(t,
		hdr("From", "Dana Okafor <dana@example.com>"),
		hdr("Reply-To", "widgets@lists.example.org"),
		hdr("To", "widgets@lists.example.org"),
		hdr("Subject", "Re: quarterly widget audit"),
	)
	plan, err := PrepareReply(context.Background(), svc, "m1", Body{Text: "unsubscribe me"})
	if err != nil {
		t.Fatalf("preparing the reply: %v", err)
	}
	if plan.To != "widgets@lists.example.org" {
		t.Errorf("To = %q, want the Reply-To", plan.To)
	}

	// And reply-all keeps it there rather than adding the sender back: the
	// Reply-To is the sender's own answer about where answers go.
	all, err := PrepareReplyAll(context.Background(), svc, "m1", Body{Text: "unsubscribe me"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}
	if all.To != "widgets@lists.example.org" {
		t.Errorf("reply-all To = %q, want the Reply-To", all.To)
	}
	if all.Cc != "" {
		t.Errorf("reply-all Cc = %q, want empty", all.Cc)
	}
}

func TestReplyAllKeepsTheAudience(t *testing.T) {
	svc := replyFixture(t,
		hdr("From", "Dana Okafor <dana@example.com>"),
		hdr("To", "ops@example.org, reader@example.com"),
		hdr("Cc", "carl@example.net, OPS@example.org"),
		hdr("Subject", "quarterly widget audit"),
	)
	plan, err := PrepareReplyAll(context.Background(), svc, "m1", Body{Text: "noted"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}
	if plan.To != "Dana Okafor <dana@example.com>" {
		t.Errorf("To = %q, want the sender", plan.To)
	}
	// To order, then Cc, each address once: ops@example.org is on the message
	// twice (in To and again in Cc, in a different case) and is one recipient,
	// and the mailbox's own address is not on the reply at all.
	if plan.Cc != "ops@example.org, carl@example.net" {
		t.Errorf("Cc = %q, want the rest of the audience, deduped, without this mailbox", plan.Cc)
	}
	if got := raw(t, plan); !strings.Contains(got, "Cc: ops@example.org, carl@example.net\r\n") {
		t.Errorf("the raw message does not carry the Cc:\n%s", got)
	}
}

func TestReplyAllDropsEveryAddressTheMailboxOwns(t *testing.T) {
	f := newFakeGmail(t, map[string]listPage{})
	f.bodies["m1"] = "the message being answered"
	f.profile = "reader@example.com"
	f.sendAs = []string{"zach@example.com", "reader@EXAMPLE.com"}
	f.headers["m1"] = []*gmail.MessagePartHeader{
		hdr("From", "Dana Okafor <dana@example.com>"),
		hdr("To", "zach@example.com, dana@example.com"),
		hdr("Cc", "reader@example.com"),
		hdr("Subject", "quarterly widget audit"),
	}
	plan, err := PrepareReplyAll(context.Background(), f.service(t), "m1", Body{Text: "noted"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}
	// The alias is the whole reason to ask the mailbox rather than guess: this
	// mail was addressed to an alias, not to the account's own name.
	if plan.To != "Dana Okafor <dana@example.com>" {
		t.Errorf("To = %q, want the sender", plan.To)
	}
	// dana@example.com is both the sender and a To; one recipient, and in To.
	if plan.Cc != "" {
		t.Errorf("Cc = %q, want empty — the aliases are the reader's own", plan.Cc)
	}
}

func TestReplyAllPromotesARecepientWhenTheSenderIsYou(t *testing.T) {
	// Answering a message you sent: To would be empty after removing yourself,
	// and a message needs one, so the first of the original recipients goes
	// there instead of the reply going nowhere.
	svc := replyFixture(t,
		hdr("From", "reader@example.com"),
		hdr("To", "dana@example.com, carl@example.net"),
		hdr("Cc", "ops@example.org"),
		hdr("Subject", "quarterly widget audit"),
	)
	plan, err := PrepareReplyAll(context.Background(), svc, "m1", Body{Text: "following up"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}
	if plan.To != "dana@example.com" {
		t.Errorf("To = %q, want the first of the original recipients", plan.To)
	}
	if plan.Cc != "carl@example.net, ops@example.org" {
		t.Errorf("Cc = %q", plan.Cc)
	}
}

func TestReplyAllAnswersTheReaderWhenTheWholeAudienceIsTheirs(t *testing.T) {
	// A note to self: taking the reader off the audience leaves nobody, and the
	// sender comes back rather than the reply being refused. Answering yourself is
	// what a reply to such a message means, and it is the only address in it.
	svc := replyFixture(t,
		hdr("From", "Zach Manson <reader@example.com>"),
		hdr("To", "reader@example.com"),
		hdr("Subject", "note to self"),
	)
	plan, err := PrepareReplyAll(context.Background(), svc, "m1", Body{Text: "and again"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}
	if plan.To != "Zach Manson <reader@example.com>" {
		t.Errorf("To = %q, want the sender, name and all", plan.To)
	}
	if plan.Cc != "" {
		t.Errorf("Cc = %q, want empty", plan.Cc)
	}
	// And it is the message on the wire, not just the summary of one.
	if got := raw(t, plan); !strings.Contains(got, "To: Zach Manson <reader@example.com>\r\n") {
		t.Errorf("the raw message does not carry the To:\n%s", got)
	}
}

func TestReplyAllAnswersTheAddressThatWroteWhenTheRestIsYoursToo(t *testing.T) {
	// The reader's own address writing to another of their own addresses. The
	// fallback is about there being nobody but the reader, not about putting the
	// reader's addresses back on the reply: the answer goes to the one that wrote.
	f := newFakeGmail(t, map[string]listPage{})
	f.bodies["m1"] = "the message being answered"
	f.profile = "reader@example.com"
	f.sendAs = []string{"other@example.com"}
	f.headers["m1"] = []*gmail.MessagePartHeader{
		hdr("From", "Zach Manson <reader@example.com>"),
		hdr("To", "other@example.com"),
		hdr("Subject", "note to self, sort of"),
	}
	plan, err := PrepareReplyAll(context.Background(), f.service(t), "m1", Body{Text: "and again"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}
	if plan.To != "Zach Manson <reader@example.com>" {
		t.Errorf("To = %q, want the address that wrote", plan.To)
	}
	if plan.Cc != "" {
		t.Errorf("Cc = %q, want empty — an alias of the reader's is not a recipient", plan.Cc)
	}
}

func TestReplyAllRefusesAHeaderItCannotRead(t *testing.T) {
	// A reply that quietly reaches fewer people than the message did is worse
	// than one that does not go out, so an unreadable To is the end of it.
	svc := replyFixture(t,
		hdr("From", "Dana Okafor <dana@example.com>"),
		hdr("To", "the whole ops team"),
		hdr("Subject", "quarterly widget audit"),
	)
	_, err := PrepareReplyAll(context.Background(), svc, "m1", Body{Text: "noted"})
	if err == nil {
		t.Fatal("an unparsable To header was accepted")
	}
	if !strings.Contains(err.Error(), "To header") {
		t.Errorf("the error does not name the header: %v", err)
	}
}

func TestReplyAllRefusesToGuessWhoTheReaderIs(t *testing.T) {
	// The identity reads are part of preparing the reply, not an optimisation:
	// without them the answer would be a CC to the reader.
	for _, tc := range []struct {
		name string
		set  func(*fakeGmail)
	}{
		{"the profile read fails", func(f *fakeGmail) { f.profileStatus = 500 }},
		{"the send-as read fails", func(f *fakeGmail) { f.sendAsStatus = 500 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGmail(t, map[string]listPage{})
			f.bodies["m1"] = "the message being answered"
			f.headers["m1"] = []*gmail.MessagePartHeader{
				hdr("From", "Dana Okafor <dana@example.com>"),
				hdr("To", "reader@example.com"),
			}
			tc.set(f)
			_, err := PrepareReplyAll(context.Background(), f.service(t), "m1", Body{Text: "noted"})
			if err == nil {
				t.Fatal("a reply-all was prepared without knowing the mailbox's own addresses")
			}
			if !strings.Contains(err.Error(), "mailbox") {
				t.Errorf("the error does not say what could not be read: %v", err)
			}
		})
	}
}

func TestReplyKeepsADisplayNameReadable(t *testing.T) {
	// The plan is what a reader checks before sending, and Go's serialiser
	// quotes every name — correct, and noise in the sentence. A name needing no
	// quoting gets none; one that does gets net/mail's.
	svc := replyFixture(t,
		hdr("From", `"Okafor, Dana" <dana@example.com>`),
		hdr("To", "reader@example.com"),
		hdr("Subject", "quarterly widget audit"),
	)
	plan, err := PrepareReply(context.Background(), svc, "m1", Body{Text: "thanks"})
	if err != nil {
		t.Fatalf("preparing the reply: %v", err)
	}
	if plan.To != `"Okafor, Dana" <dana@example.com>` {
		t.Errorf("To = %q, want the name quoted because the comma needs it", plan.To)
	}
}

func TestBuildRawMessageRefusesALineBreakInAHeader(t *testing.T) {
	// A header built out of somebody else's header is the one place a newline
	// could become a header of its own. The addresses are re-serialised from
	// parsed values, so this is a second lock on a door already shut — and the
	// one that holds if a future caller hands in text instead.
	for _, tc := range []rawMessageOptions{
		{To: "dana@example.com\r\nBcc: someone@example.com", Subject: "hi"},
		{To: "dana@example.com", Cc: "carl@example.net\nBcc: someone@example.com", Subject: "hi"},
		{To: "dana@example.com", InReplyTo: "<m@example.com>\r\nBcc: someone@example.com", Subject: "hi"},
	} {
		if _, err := buildRawMessage(tc); err == nil {
			t.Errorf("a line break was written into a header: %+v", tc)
		}
	}

	// A subject is not one of those headers: it is encoded as one word, so a
	// newline in it becomes characters in a subject rather than a header —
	// which is why the check above is on the assembled value and not on the
	// caller's text.
	encoded, err := buildRawMessage(rawMessageOptions{
		To: "dana@example.com", Subject: "hi\nBcc: someone@example.com",
	})
	if err != nil {
		t.Fatalf("a subject containing a newline was refused: %v", err)
	}
	decoded, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(encoded)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	subject := strings.SplitN(string(decoded), "\r\n", 2)[1]
	if !strings.HasPrefix(subject, "Subject: =?UTF-8?") {
		t.Errorf("the subject was not encoded: %q", subject)
	}
	if strings.Contains(string(decoded), "\r\nBcc: someone@example.com") {
		t.Errorf("the newline in the subject became a header:\n%s", decoded)
	}
}

func TestPrepareSendStillChecksTheRecipients(t *testing.T) {
	if _, err := PrepareSend("not an address", "hi", Body{Text: "body"}); err == nil {
		t.Fatal("PrepareSend accepted a recipient that is not an address")
	}
	plan, err := PrepareSend("Dana Okafor <dana@example.com>", "hi", Body{Text: "body"})
	if err != nil {
		t.Fatalf("preparing a send: %v", err)
	}
	// A send names its own recipients and has no Reply to derive anything from:
	// the plan carries what it was given and no Cc.
	if plan.To != "Dana Okafor <dana@example.com>" || plan.Cc != "" {
		t.Errorf("To/Cc = %q/%q", plan.To, plan.Cc)
	}
}

func TestAMessageWithHTMLIsTwoAlternativeParts(t *testing.T) {
	plan, err := PrepareSend("dana@example.com", "hi", Body{
		Text: "the plain one",
		HTML: "<div>the html one</div>",
	})
	if err != nil {
		t.Fatalf("preparing the send: %v", err)
	}
	// The plan shows both forms, so a preview of a message that will go out as
	// two parts shows the two parts rather than one of them.
	if plan.Body != "the plain one" || plan.HTML != "<div>the html one</div>" {
		t.Errorf("the plan's bodies = %q/%q", plan.Body, plan.HTML)
	}

	msg := raw(t, plan)
	if !strings.Contains(msg, "Content-Type: multipart/alternative; boundary=\"=_docket_0_=\"") {
		t.Errorf("the message is not multipart/alternative:\n%s", msg)
	}
	// The header block ends where the body begins, and the framing is the only
	// thing between the parts.
	head, body, found := strings.Cut(msg, "\r\n\r\n")
	if !found {
		t.Fatalf("no header/body split:\n%s", msg)
	}
	if strings.Contains(head, "text/plain") {
		t.Errorf("a multipart has no top-level content type of its own:\n%s", head)
	}
	want := "--=_docket_0_=\r\n" +
		"Content-Type: text/plain; charset=\"UTF-8\"\r\n" +
		"\r\n" +
		"the plain one\r\n" +
		"--=_docket_0_=\r\n" +
		"Content-Type: text/html; charset=\"UTF-8\"\r\n" +
		"\r\n" +
		"<div>the html one</div>\r\n" +
		"--=_docket_0_=--\r\n"
	if body != want {
		t.Errorf("the parts are not as expected:\ngot:\n%q\nwant:\n%q", body, want)
	}
}

func TestThePartsAreSeparatedByCRLFRatherThanByTheText(t *testing.T) {
	// A body that already ends in a line break is the ordinary case — a caller
	// quoting a message hands in text ending in one — and the framing must not
	// turn it into a blank line inside the part.
	one := raw(t, mustSend(t, Body{Text: "line\n", HTML: "<div>line</div>\n"}))
	two := raw(t, mustSend(t, Body{Text: "line", HTML: "<div>line</div>"}))
	if one != two {
		t.Errorf("a trailing line break changed the message:\n%q\n%q", one, two)
	}
	if strings.Contains(one, "\n\n\n") {
		t.Errorf("a blank line was left inside a part:\n%q", one)
	}
	// And a text-only message is still one part, byte for byte as before.
	plain := raw(t, mustSend(t, Body{Text: "line"}))
	if strings.Contains(plain, "multipart") || strings.Count(plain, "Content-Type") != 1 {
		t.Errorf("a body without HTML is not a single part:\n%s", plain)
	}
}

func TestTheBoundaryIsNotSomethingTheMessageContains(t *testing.T) {
	// A boundary occurring inside a part ends it there, so the token is chosen
	// against the text rather than assumed to be absent from it — the message
	// here is the one that contains the first candidate.
	msg := raw(t, mustSend(t, Body{
		Text: "what does =_docket_0_= mean",
		HTML: "<div>and =_docket_1_= too</div>",
	}))
	if !strings.Contains(msg, "Content-Type: multipart/alternative; boundary=\"=_docket_2_=\"") {
		t.Errorf("the boundary was not moved past the text:\n%s", msg)
	}
	if strings.Count(msg, "--=_docket_2_=") != 3 {
		t.Errorf("the chosen boundary is not the only one framing the parts:\n%s", msg)
	}
}

// mustSend is the plan a send builds, or a failed test: the assertions above are
// about the bytes, and a plan that could not be built has none.
func mustSend(t *testing.T, body Body) *SendPlan {
	t.Helper()
	plan, err := PrepareSend("dana@example.com", "hi", body)
	if err != nil {
		t.Fatalf("preparing the send: %v", err)
	}
	return plan
}

// The audience a plan can be narrowed to: the addresses the message carried, in
// the two lists a reply puts them in. What is checked here is that the raw
// message is built from the chosen set rather than patched — and that a caller
// may name fewer addresses than the message carried, which is the other half of
// naming the audience outright (see TestWithRecipientsWidensTheAudience…).
func TestWithRecipientsNarrowsTheAudienceAndRebuildsTheMessage(t *testing.T) {
	svc := replyFixture(t,
		hdr("From", "Dana Okafor <dana@example.com>"),
		hdr("To", "reader@example.com, carl@example.net"),
		hdr("Cc", "ops@example.org"),
		hdr("Subject", "quarterly widget audit"),
	)
	plan, err := PrepareReplyAll(context.Background(), svc, "m1", Body{Text: "noted"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}
	// The plan holds the audience as addresses, with the names the message gave
	// them, in the order it carried them.
	if len(plan.ToRecipients) != 1 || plan.ToRecipients[0] != (Recipient{Name: "Dana Okafor", Address: "dana@example.com"}) {
		t.Errorf("ToRecipients = %+v", plan.ToRecipients)
	}
	if len(plan.CcRecipients) != 2 || plan.CcRecipients[0].Address != "carl@example.net" || plan.CcRecipients[1].Address != "ops@example.org" {
		t.Errorf("CcRecipients = %+v", plan.CcRecipients)
	}

	// Untick ops: it was on the message, and the message that goes out no longer
	// carries it — not in the header, and not in the bytes either.
	narrowed, err := plan.WithRecipients(
		[]string{"dana@example.com"},
		[]string{"carl@example.net"},
	)
	if err != nil {
		t.Fatalf("narrowing the reply: %v", err)
	}
	if narrowed.Cc != "carl@example.net" {
		t.Errorf("Cc = %q, want the one address left", narrowed.Cc)
	}
	if got := raw(t, narrowed); strings.Contains(got, "ops@example.org") {
		t.Errorf("the dropped address is still in the message:\n%s", got)
	}
	if got := raw(t, narrowed); !strings.Contains(got, "Cc: carl@example.net\r\n") {
		t.Errorf("the message does not carry the narrowed Cc:\n%s", got)
	}
	// The plan it came from is untouched: a second narrowing starts from the
	// message's whole audience rather than from the first result.
	if plan.Cc != "carl@example.net, ops@example.org" {
		t.Errorf("the original plan was mutated: Cc = %q", plan.Cc)
	}
}

func TestWithRecipientsMovesAnAddressBetweenToAndCc(t *testing.T) {
	svc := replyFixture(t,
		hdr("From", "Dana Okafor <dana@example.com>"),
		hdr("To", "reader@example.com"),
		hdr("Cc", "carl@example.net"),
		hdr("Subject", "quarterly widget audit"),
	)
	plan, err := PrepareReplyAll(context.Background(), svc, "m1", Body{Text: "noted"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}
	moved, err := plan.WithRecipients(
		[]string{"dana@example.com", "carl@example.net"},
		nil,
	)
	if err != nil {
		t.Fatalf("moving the recipient: %v", err)
	}
	if moved.To != "Dana Okafor <dana@example.com>, carl@example.net" {
		t.Errorf("To = %q, want both addresses in the message's own order", moved.To)
	}
	if moved.Cc != "" {
		t.Errorf("Cc = %q, want empty", moved.Cc)
	}
	got := raw(t, moved)
	if strings.Contains(got, "Cc:") {
		t.Errorf("a message with nobody in Cc carries a Cc header:\n%s", got)
	}
	if !strings.Contains(got, "To: Dana Okafor <dana@example.com>, carl@example.net\r\n") {
		t.Errorf("the moved address is not in To:\n%s", got)
	}
	// The display name follows the address it was carried with.
	if len(moved.ToRecipients) != 2 || moved.ToRecipients[1] != (Recipient{Address: "carl@example.net"}) {
		t.Errorf("ToRecipients = %+v", moved.ToRecipients)
	}
}

// The audience a plan may be given, including addresses the message being
// answered never carried: a caller names who the reply goes to, and this is
// where that is carried out rather than refused.
func TestWithRecipientsWidensTheAudienceAndRebuildsTheMessage(t *testing.T) {
	svc := replyFixture(t,
		hdr("From", "Dana Okafor <dana@example.com>"),
		hdr("To", "reader@example.com"),
		hdr("Subject", "quarterly widget audit"),
	)
	plan, err := PrepareReplyAll(context.Background(), svc, "m1", Body{Text: "noted"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}

	// One address typed by hand, and one a caller knows the name of. Neither was
	// on the message, and both are on the reply — in the plan, in the header, and
	// in the bytes of the message that would go out.
	widened, err := plan.WithRecipients(
		[]string{"dana@example.com", "stranger@example.com"},
		[]string{"Ada Okoye <ada@example.net>"},
	)
	if err != nil {
		t.Fatalf("naming the reply's audience: %v", err)
	}
	if widened.To != "Dana Okafor <dana@example.com>, stranger@example.com" {
		t.Errorf("To = %q, want the sender and the typed address", widened.To)
	}
	if widened.Cc != "Ada Okoye <ada@example.net>" {
		t.Errorf("Cc = %q, want the named address", widened.Cc)
	}
	got := raw(t, widened)
	if !strings.Contains(got, "To: Dana Okafor <dana@example.com>, stranger@example.com\r\n") {
		t.Errorf("the widened To is not in the message:\n%s", got)
	}
	if !strings.Contains(got, "Cc: Ada Okoye <ada@example.net>\r\n") {
		t.Errorf("the widened Cc is not in the message:\n%s", got)
	}
	// Still a reply to the message it was prepared from: widening the audience
	// rebuilds the message and does not lose what threaded it.
	if !strings.Contains(got, "In-Reply-To: <m1@mail.example.com>\r\n") {
		t.Errorf("the widened reply lost its threading:\n%s", got)
	}

	// An address that was the message's own keeps the name the message gave it,
	// and one the message did not carry is carried as the caller wrote it.
	if len(widened.ToRecipients) != 2 ||
		widened.ToRecipients[0] != (Recipient{Name: "Dana Okafor", Address: "dana@example.com"}) ||
		widened.ToRecipients[1] != (Recipient{Address: "stranger@example.com"}) {
		t.Errorf("ToRecipients = %+v", widened.ToRecipients)
	}
	if len(widened.CcRecipients) != 1 ||
		widened.CcRecipients[0] != (Recipient{Name: "Ada Okoye", Address: "ada@example.net"}) {
		t.Errorf("CcRecipients = %+v", widened.CcRecipients)
	}

	// The plan it came from is untouched, and the widened plan is one a caller can
	// narrow again: its added addresses are its own recipients now.
	if plan.To != "Dana Okafor <dana@example.com>" || plan.Cc != "" {
		t.Errorf("the original plan was mutated: To = %q, Cc = %q", plan.To, plan.Cc)
	}
	if _, err := widened.WithRecipients([]string{"stranger@example.com"}, nil); err != nil {
		t.Errorf("the widened audience could not be narrowed again: %v", err)
	}
}

func TestWithRecipientsRefusesAnEmptyOrDoubledTo(t *testing.T) {
	svc := replyFixture(t,
		hdr("From", "Dana Okafor <dana@example.com>"),
		hdr("To", "reader@example.com"),
		hdr("Cc", "carl@example.net"),
		hdr("Subject", "quarterly widget audit"),
	)
	plan, err := PrepareReplyAll(context.Background(), svc, "m1", Body{Text: "noted"})
	if err != nil {
		t.Fatalf("preparing the reply-all: %v", err)
	}
	if _, err := plan.WithRecipients(nil, []string{"carl@example.net"}); err == nil {
		t.Error("a reply with nobody in To was accepted")
	}
	// One address in both lists, or twice in one, is one recipient said twice —
	// and would be a message with the same person on it two ways. Which list an
	// address is in is the caller's to choose; being in two of them is not a
	// choice, and neither list is widened by refusing it.
	if _, err := plan.WithRecipients(
		[]string{"dana@example.com"},
		[]string{"Dana@Example.com"},
	); err == nil {
		t.Error("an address in both To and Cc was accepted")
	}
	if _, err := plan.WithRecipients(
		[]string{"dana@example.com", "Dana@Example.com"},
		nil,
	); err == nil {
		t.Error("an address twice in To was accepted")
	}
	// The shape of an address is the other thing that is still checked, for the
	// same reason: what cannot be parsed cannot be written into a header.
	if _, err := plan.WithRecipients([]string{"not an address"}, nil); err == nil {
		t.Error("a string that is not an address was accepted")
	}
}

func TestPrepareSendCarriesItsRecipientsAsAddresses(t *testing.T) {
	plan, err := PrepareSend("Dana Okafor <dana@example.com>, carl@example.net", "hi", Body{Text: "body"})
	if err != nil {
		t.Fatalf("preparing a send: %v", err)
	}
	if len(plan.ToRecipients) != 2 ||
		plan.ToRecipients[0] != (Recipient{Name: "Dana Okafor", Address: "dana@example.com"}) ||
		plan.ToRecipients[1] != (Recipient{Address: "carl@example.net"}) {
		t.Errorf("ToRecipients = %+v", plan.ToRecipients)
	}
	if len(plan.CcRecipients) != 0 {
		t.Errorf("CcRecipients = %+v, want none", plan.CcRecipients)
	}
}
