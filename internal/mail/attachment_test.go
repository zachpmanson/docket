package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
)

// pngBytes is a PNG signature and nothing else: eight bytes, enough for a
// test to prove the file on disk is the bytes Gmail returned rather than a
// re-encoding of them. No real attachment belongs in a fixture.
var pngBytes = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// attachmentPart is a filename-bearing part whose content lives behind the
// separate attachments.get call, which is how Gmail returns anything but the
// smallest parts.
func attachmentPart(partID, attachmentID, filename, mimeType string, size int64) *gmail.MessagePart {
	return &gmail.MessagePart{
		MimeType: mimeType,
		PartId:   partID,
		Filename: filename,
		Body:     &gmail.MessagePartBody{Size: size, AttachmentId: attachmentID},
	}
}

// fetchFixture serves one payload and fetches a part out of it, into a
// temporary directory the test owns.
func fetchFixture(t *testing.T, payload *gmail.MessagePart, opts FetchOptions,
	setup func(*fakeGmail)) (*FetchedAttachment, string, error) {
	t.Helper()
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.payloads["m1"] = payload
	if setup != nil {
		setup(f)
	}
	if opts.OutPath == "" {
		opts.OutPath = filepath.Join(t.TempDir(), "fetched.bin")
	}
	got, err := FetchAttachment(context.Background(), f.service(t), "m1", opts)
	return got, opts.OutPath, err
}

// imagePayload is the shape an inline screenshot arrives in: an html part and
// the image it references, wrapped in a multipart/mixed beside a document, so
// the part chosen has to be chosen by id rather than by position.
func imagePayload() *gmail.MessagePart {
	inline := attachmentPart("1.2", "att-img", "screenshot.png", "image/png", int64(len(pngBytes)))
	inline.Headers = []*gmail.MessagePartHeader{{Name: "Content-ID", Value: "<ii-9f3c2a@mail.example.com>"}}
	return multipart("multipart/mixed",
		multipart("multipart/related", htmlPart(htmlSource), inline),
		attachmentPart("2", "att-doc", "quote.pdf", "application/pdf", 9000))
}

func TestFetchWritesTheBytesUnmodified(t *testing.T) {
	// Priority one for the consumer: it decides whether to thumbnail, and it
	// cannot decide that from bytes docket has already re-encoded.
	got, path, err := fetchFixture(t, imagePayload(), FetchOptions{PartID: "1.2", MaxBytes: NoMaxBytes},
		func(f *fakeGmail) { f.attachments["att-img"] = pngBytes })
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the written file: %v", err)
	}
	if string(onDisk) != string(pngBytes) {
		t.Errorf("file holds %#v, want the source bytes %#v", onDisk, pngBytes)
	}
	if got.Path != path {
		t.Errorf("path = %q, want %q", got.Path, path)
	}
	if got.Size != int64(len(pngBytes)) {
		t.Errorf("size = %d, want %d", got.Size, len(pngBytes))
	}
	if got.MimeType != "image/png" || got.Filename != "screenshot.png" {
		t.Errorf("mime_type/filename = %q/%q, want image/png/screenshot.png", got.MimeType, got.Filename)
	}
	sum := sha256.Sum256(pngBytes)
	if got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want the digest of the bytes written", got.SHA256)
	}
}

func TestFetchReportsTheContentIDForAnInlineImage(t *testing.T) {
	// A cid: URL in the html body names the Content-ID, not the part id. With
	// the brackets left on, or the field absent, a consumer holding
	// "cid:ii-9f3c2a@mail.example.com" cannot say which part to fetch and the
	// image stays broken with nothing reported anywhere.
	got, _, err := fetchFixture(t, imagePayload(), FetchOptions{PartID: "1.2", MaxBytes: NoMaxBytes},
		func(f *fakeGmail) { f.attachments["att-img"] = pngBytes })
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if got.ContentID != "ii-9f3c2a@mail.example.com" {
		t.Errorf("content_id = %q, want the bare token a cid: URL carries", got.ContentID)
	}
}

func TestFetchSelectsThePartByID(t *testing.T) {
	// Two attachments in one message, and the wrong choice is silent: the
	// caller gets a file, just not the one it asked for.
	got, path, err := fetchFixture(t, imagePayload(), FetchOptions{PartID: "2", MaxBytes: NoMaxBytes},
		func(f *fakeGmail) {
			f.attachments["att-img"] = pngBytes
			f.attachments["att-doc"] = []byte("%PDF-1.7")
		})
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if got.Filename != "quote.pdf" {
		t.Errorf("filename = %q, want quote.pdf", got.Filename)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the written file: %v", err)
	}
	if string(onDisk) != "%PDF-1.7" {
		t.Errorf("file holds %q, want the pdf part's bytes", onDisk)
	}
}

func TestFetchReadsAPartWithInlineData(t *testing.T) {
	// Gmail returns small parts in the message itself, with no attachment id
	// and so no second endpoint to call. Requiring an attachment id would
	// make exactly the smallest attachments unfetchable.
	part := &gmail.MessagePart{
		MimeType: "text/csv", PartId: "1", Filename: "rates.csv",
		Body: partBody("site,rate\nICP-0001,0.1234\n"),
	}

	got, path, err := fetchFixture(t, part, FetchOptions{PartID: "1", MaxBytes: NoMaxBytes}, nil)
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the written file: %v", err)
	}
	if string(onDisk) != "site,rate\nICP-0001,0.1234\n" {
		t.Errorf("file holds %q, want the inline part's bytes", onDisk)
	}
	if got.Size != int64(len(onDisk)) {
		t.Errorf("size = %d, want %d", got.Size, len(onDisk))
	}
}

func TestFetchCapRefusesBeforeDownloading(t *testing.T) {
	// The point of the cap for a caller fetching thousands: an oversized
	// attachment must cost one metadata request, not the download it was
	// never going to keep.
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.payloads["m1"] = multipart("multipart/mixed",
		attachmentPart("1", "att-big", "recording.mov", "video/quicktime", 40<<20))
	f.attachments["att-big"] = pngBytes
	path := filepath.Join(t.TempDir(), "fetched.bin")

	_, err := FetchAttachment(context.Background(), f.service(t), "m1",
		FetchOptions{PartID: "1", MaxBytes: 1 << 20, OutPath: path})

	if !errors.Is(err, ErrAttachmentTooLarge) {
		t.Fatalf("err = %v, want ErrAttachmentTooLarge", err)
	}
	if len(f.attachmentRequests) != 0 {
		t.Errorf("made %d content calls for a refused fetch, want 0: %v",
			len(f.attachmentRequests), f.attachmentRequests)
	}
	if !strings.Contains(err.Error(), "41943040") || !strings.Contains(err.Error(), "1048576") {
		t.Errorf("error %q names neither the size nor the cap, so a caller cannot tell how much to raise it", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("a refused fetch left a file at %q", path)
	}
}

func TestFetchCapEqualToTheSizeSucceeds(t *testing.T) {
	// Off-by-one here refuses a file that fits, and a bulk caller sizing its
	// cap from the size `read` reported would see every fetch fail.
	got, _, err := fetchFixture(t, imagePayload(),
		FetchOptions{PartID: "1.2", MaxBytes: len(pngBytes)},
		func(f *fakeGmail) { f.attachments["att-img"] = pngBytes })
	if err != nil {
		t.Fatalf("FetchAttachment with the cap at the exact size: %v", err)
	}
	if got.Size != int64(len(pngBytes)) {
		t.Errorf("size = %d, want %d", got.Size, len(pngBytes))
	}
}

func TestFetchCapOneBelowTheSizeRefuses(t *testing.T) {
	_, _, err := fetchFixture(t, imagePayload(),
		FetchOptions{PartID: "1.2", MaxBytes: len(pngBytes) - 1},
		func(f *fakeGmail) { f.attachments["att-img"] = pngBytes })

	if !errors.Is(err, ErrAttachmentTooLarge) {
		t.Fatalf("err = %v, want ErrAttachmentTooLarge", err)
	}
}

func TestFetchNoCapFetchesAnySize(t *testing.T) {
	// 0 means unlimited, the same as it does for --max-bytes on read; a
	// second convention for the same flag is how a caller ends up guessing a
	// number large enough.
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.payloads["m1"] = attachmentPart("1", "att-big", "plans.pdf", "application/pdf", 40<<20)
	f.attachments["att-big"] = pngBytes

	got, err := FetchAttachment(context.Background(), f.service(t), "m1",
		FetchOptions{PartID: "1", MaxBytes: NoMaxBytes, OutPath: filepath.Join(t.TempDir(), "f.bin")})
	if err != nil {
		t.Fatalf("FetchAttachment with no cap: %v", err)
	}
	if got.Size != int64(len(pngBytes)) {
		t.Errorf("size = %d, want the bytes actually received", got.Size)
	}
}

func TestFetchCapAppliesToTheBytesReceived(t *testing.T) {
	// The metadata size is Gmail's claim. A part that understates itself and
	// is checked only before the download would write a file the caller
	// explicitly capped against.
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.payloads["m1"] = attachmentPart("1", "att-liar", "small.png", "image/png", 4)
	f.attachments["att-liar"] = pngBytes
	path := filepath.Join(t.TempDir(), "fetched.bin")

	_, err := FetchAttachment(context.Background(), f.service(t), "m1",
		FetchOptions{PartID: "1", MaxBytes: 4, OutPath: path})

	if !errors.Is(err, ErrAttachmentTooLarge) {
		t.Fatalf("err = %v, want ErrAttachmentTooLarge", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("a refused fetch left a file at %q", path)
	}
}

func TestFetchRejectsANegativeCap(t *testing.T) {
	_, _, err := fetchFixture(t, imagePayload(), FetchOptions{PartID: "1.2", MaxBytes: -1}, nil)

	if !errors.Is(err, ErrNegativeAttachmentMaxBytes) {
		t.Fatalf("err = %v, want ErrNegativeAttachmentMaxBytes", err)
	}
}

func TestFetchRequiresAnOutputPath(t *testing.T) {
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.payloads["m1"] = imagePayload()

	_, err := FetchAttachment(context.Background(), f.service(t), "m1", FetchOptions{PartID: "1.2"})
	if err == nil {
		t.Fatal("a fetch with no --out succeeded; the bytes would have had nowhere to go")
	}
	if len(f.attachmentRequests) != 0 {
		t.Errorf("made a content call for a fetch that could not be written: %v", f.attachmentRequests)
	}
}

// TestFetchDistinguishesTheThreeMissingCases is the outcome the consumer asked
// for: it walks metadata that may be months old, so each of these is a normal
// answer, and one code covering all three would tell it nothing it can act on.
func TestFetchDistinguishesTheThreeMissingCases(t *testing.T) {
	t.Run("message does not exist", func(t *testing.T) {
		f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})

		_, err := FetchAttachment(context.Background(), f.service(t), "m-gone",
			FetchOptions{PartID: "1", MaxBytes: NoMaxBytes, OutPath: filepath.Join(t.TempDir(), "f.bin")})
		if err == nil {
			t.Fatal("fetching from a message that does not exist succeeded")
		}
		if got := Classify(err).Code; got != CodeNotFound {
			t.Errorf("Classify = %q, want %q", got, CodeNotFound)
		}
		if errors.Is(err, ErrPartNotFound) || errors.Is(err, ErrAttachmentUnavailable) {
			t.Errorf("a deleted message reports as a part-level failure: %v", err)
		}
	})

	t.Run("part id not in the message", func(t *testing.T) {
		_, _, err := fetchFixture(t, imagePayload(),
			FetchOptions{PartID: "7.4", MaxBytes: NoMaxBytes}, nil)

		if !errors.Is(err, ErrPartNotFound) {
			t.Fatalf("err = %v, want ErrPartNotFound", err)
		}
		// The alternatives are what turn a wrong call into a corrected one.
		if !strings.Contains(err.Error(), "[1.2 2]") {
			t.Errorf("error %q does not name the part ids the message does have", err)
		}
	})

	t.Run("part exists with no content behind it", func(t *testing.T) {
		payload := multipart("multipart/mixed",
			&gmail.MessagePart{
				MimeType: "image/png", PartId: "1", Filename: "stripped.png",
				Body: &gmail.MessagePartBody{Size: 0},
			})

		_, _, err := fetchFixture(t, payload, FetchOptions{PartID: "1", MaxBytes: NoMaxBytes}, nil)

		if !errors.Is(err, ErrAttachmentUnavailable) {
			t.Fatalf("err = %v, want ErrAttachmentUnavailable", err)
		}
		if errors.Is(err, ErrPartNotFound) {
			t.Errorf("a part with no content reports as a part that is not there: %v", err)
		}
	})
}

// TestFetchRateLimitIsNotAMissingPart is the failure a bulk fetch actually
// hits. Reported as a missing part it would be recorded as a permanent gap in
// the corpus and never asked for again.
func TestFetchRateLimitIsNotAMissingPart(t *testing.T) {
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})
	f.payloads["m1"] = imagePayload()
	f.attachmentStatus["att-img"] = http.StatusTooManyRequests

	_, err := FetchAttachment(context.Background(), f.service(t), "m1",
		FetchOptions{PartID: "1.2", MaxBytes: NoMaxBytes, OutPath: filepath.Join(t.TempDir(), "f.bin")})
	if err == nil {
		t.Fatal("a rate-limited fetch succeeded")
	}

	got := Classify(err)
	if got.Code != CodeRateLimited {
		t.Errorf("Classify = %q, want %q", got.Code, CodeRateLimited)
	}
	if !got.Retryable {
		t.Errorf("retryable = false for a rate limit; a bulk fetch has nothing to back off on")
	}
	if errors.Is(err, ErrPartNotFound) || errors.Is(err, ErrAttachmentUnavailable) {
		t.Errorf("a rate limit reports as a missing part: %v", err)
	}
}

func TestFetchToAnUnwritableDirectoryIsAWriteFailure(t *testing.T) {
	// A local write failure says nothing about the mailbox. Classified as a
	// missing attachment it would have the caller record a gap that a second
	// run would never revisit.
	_, _, err := fetchFixture(t, imagePayload(),
		FetchOptions{PartID: "1.2", MaxBytes: NoMaxBytes,
			OutPath: filepath.Join(t.TempDir(), "no-such-dir", "f.bin")},
		func(f *fakeGmail) { f.attachments["att-img"] = pngBytes })

	if !errors.Is(err, ErrOutputWrite) {
		t.Fatalf("err = %v, want ErrOutputWrite", err)
	}
}

func TestFetchOverwritesAnEarlierFetchOfTheSamePart(t *testing.T) {
	// A resumed backfill re-fetches parts it already has; refusing would make
	// the second run fail on every one of them.
	dir := t.TempDir()
	path := filepath.Join(dir, "fetched.bin")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seeding the destination: %v", err)
	}

	_, _, err := fetchFixture(t, imagePayload(),
		FetchOptions{PartID: "1.2", MaxBytes: NoMaxBytes, OutPath: path},
		func(f *fakeGmail) { f.attachments["att-img"] = pngBytes })
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the written file: %v", err)
	}
	if string(onDisk) != string(pngBytes) {
		t.Errorf("file holds %q, want the freshly fetched bytes", onDisk)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing the output directory: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("output directory holds %d files, want just the attachment: %v", len(entries), entries)
	}
}
