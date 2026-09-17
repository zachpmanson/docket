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
	plan, err := PrepareReply(context.Background(), svc, "m1", "thanks")
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
	plan, err := PrepareReply(context.Background(), svc, "m1", "unsubscribe me")
	if err != nil {
		t.Fatalf("preparing the reply: %v", err)
	}
	if plan.To != "widgets@lists.example.org" {
		t.Errorf("To = %q, want the Reply-To", plan.To)
	}

	// And reply-all keeps it there rather than adding the sender back: the
	// Reply-To is the sender's own answer about where answers go.
	all, err := PrepareReplyAll(context.Background(), svc, "m1", "unsubscribe me")
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
	plan, err := PrepareReplyAll(context.Background(), svc, "m1", "noted")
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
	plan, err := PrepareReplyAll(context.Background(), f.service(t), "m1", "noted")
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
	plan, err := PrepareReplyAll(context.Background(), svc, "m1", "following up")
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

func TestReplyAllRefusesAMessageOnlyThisMailboxIsOn(t *testing.T) {
	svc := replyFixture(t,
		hdr("From", "reader@example.com"),
		hdr("To", "reader@example.com"),
		hdr("Subject", "note to self"),
	)
	_, err := PrepareReplyAll(context.Background(), svc, "m1", "and again")
	if err == nil {
		t.Fatal("a reply-all with nobody but this mailbox on it was prepared")
	}
	if !strings.Contains(err.Error(), "nobody to reply to") {
		t.Errorf("the error does not say what is wrong: %v", err)
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
	_, err := PrepareReplyAll(context.Background(), svc, "m1", "noted")
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
			_, err := PrepareReplyAll(context.Background(), f.service(t), "m1", "noted")
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
	plan, err := PrepareReply(context.Background(), svc, "m1", "thanks")
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
	if _, err := PrepareSend("not an address", "hi", "body"); err == nil {
		t.Fatal("PrepareSend accepted a recipient that is not an address")
	}
	plan, err := PrepareSend("Dana Okafor <dana@example.com>", "hi", "body")
	if err != nil {
		t.Fatalf("preparing a send: %v", err)
	}
	// A send names its own recipients and has no Reply to derive anything from:
	// the plan carries what it was given and no Cc.
	if plan.To != "Dana Okafor <dana@example.com>" || plan.Cc != "" {
		t.Errorf("To/Cc = %q/%q", plan.To, plan.Cc)
	}
}
