package mail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/api/gmail/v1"
)

// DefaultAttachmentMaxBytes caps a fetch when the caller does not say
// otherwise. It sits well above the screenshots and PDFs that make up almost
// all mail attachments and well below the 25 MB Gmail itself accepts, so the
// default costs a bulk fetcher nothing and still stops one accidental video
// from being pulled down a thousand times.
const DefaultAttachmentMaxBytes = 10 << 20

// Attachment fetch failures a caller branches on. Each is a normal outcome of
// walking metadata that may be months old, and each demands a different
// response, so none of them may collapse into the others or into the
// network/rate-limit classes Classify returns.
var (
	// ErrPartNotFound: the message exists and no part in it has that part id.
	// The metadata the caller is holding does not describe this message any
	// more — re-read it rather than retrying the fetch.
	ErrPartNotFound = errors.New("no such part in this message")

	// ErrAttachmentUnavailable: the part exists and carries no content —
	// Gmail returns neither an attachment id nor inline data. Nothing else to
	// fetch, so this is terminal for that part while the message around it is
	// still fine.
	ErrAttachmentUnavailable = errors.New("this part carries no attachment content")

	// ErrAttachmentTooLarge: the part is bigger than the cap. Refused rather
	// than truncated — see FetchAttachment.
	ErrAttachmentTooLarge = errors.New("attachment is larger than --max-bytes")

	// ErrOutputWrite: the bytes arrived and the local write failed. Nothing
	// about the mailbox is wrong, so a caller that treats it as a missing
	// attachment would record a permanent gap for a full disk or a typo'd
	// directory.
	ErrOutputWrite = errors.New("writing the attachment to disk failed")

	// ErrNegativeAttachmentMaxBytes mirrors ErrNegativeMaxBytes: 0 already
	// means unlimited, so a negative has no reading left that is not a guess.
	ErrNegativeAttachmentMaxBytes = errors.New("--max-bytes must be 0 (unlimited) or a positive byte count")
)

// FetchOptions are the per-call knobs for FetchAttachment.
//
// OutPath is required. Bytes go to a file rather than to stdout because
// stdout carries the JSON envelope every docket command emits, and a caller
// that has to find the envelope inside a stream of PNG bytes cannot read a
// failure at all — see docket-design.md §4.
//
// MaxBytes follows the --max-bytes convention: 0 (NoMaxBytes) is unlimited.
type FetchOptions struct {
	PartID   string
	MaxBytes int
	OutPath  string
}

// FetchedAttachment is what a successful fetch reports: where the bytes
// landed, and enough about them for a caller to file them without a second
// call.
//
// SHA256 is over the decoded bytes. A corpus of thousands of screenshots
// repeats itself heavily, and the digest is what lets a caller store one copy
// and confirm that a re-fetch produced the same file.
type FetchedAttachment struct {
	MessageID string `json:"message_id"`
	PartID    string `json:"part_id"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mime_type"`
	ContentID string `json:"content_id,omitempty"`
	Size      int64  `json:"size"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
}

// FetchAttachment writes one attachment's bytes to opts.OutPath and reports
// where they went. The bytes are the sender's, byte for byte: no resizing, no
// re-encoding, no format sniffing. A consumer deciding whether to thumbnail
// an image needs the original to decide from.
//
// The cap is enforced against the size Gmail reports in the message metadata
// before the content call is made, so an oversized attachment costs one cheap
// request instead of a download the caller was never going to keep. It is
// also enforced against the bytes actually received, because the metadata
// size is Gmail's claim rather than a measurement.
//
// Over the cap is a failure, not a truncation. Half a PNG is not a smaller
// PNG — it is a file that every decoder rejects, and one that would be
// indistinguishable from a complete file once written to disk and forgotten
// about.
func FetchAttachment(ctx context.Context, svc *gmail.Service, id string, opts FetchOptions) (*FetchedAttachment, error) {
	if opts.MaxBytes < 0 {
		return nil, fmt.Errorf("--max-bytes %d: %w", opts.MaxBytes, ErrNegativeAttachmentMaxBytes)
	}
	if opts.OutPath == "" {
		return nil, errors.New("--out is required: an attachment's bytes are written to a file, not to stdout")
	}

	msg, err := svc.Users.Messages.Get(meUser, id).Format("full").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf(
			"fetching message %q: %w (docket mail ids come from `mail search`/`mail list` output — "+
				"confirm this id was copied from there, not an IMAP UID or sequence number)", id, err)
	}

	part := findPart(msg.Payload, opts.PartID)
	if part == nil {
		return nil, fmt.Errorf("part %q of message %q: %w (parts carrying an attachment in this message: %v; "+
			"part ids come from the attachments list in `mail read` output)",
			opts.PartID, id, ErrPartNotFound, attachmentPartIDs(msg.Payload))
	}
	if part.Body == nil || (part.Body.AttachmentId == "" && part.Body.Data == "") {
		return nil, fmt.Errorf("part %q of message %q (%s): %w",
			opts.PartID, id, part.MimeType, ErrAttachmentUnavailable)
	}
	if err := checkAttachmentSize(part.Body.Size, opts.MaxBytes, opts.PartID, id); err != nil {
		return nil, err
	}

	data, err := attachmentData(ctx, svc, id, part)
	if err != nil {
		return nil, err
	}
	if err := checkAttachmentSize(int64(len(data)), opts.MaxBytes, opts.PartID, id); err != nil {
		return nil, err
	}

	if err := writeFileAtomic(opts.OutPath, data); err != nil {
		return nil, err
	}

	sum := sha256.Sum256(data)
	return &FetchedAttachment{
		MessageID: id,
		PartID:    opts.PartID,
		Filename:  part.Filename,
		MimeType:  part.MimeType,
		ContentID: contentID(part),
		Size:      int64(len(data)),
		Path:      opts.OutPath,
		SHA256:    hex.EncodeToString(sum[:]),
	}, nil
}

// attachmentData returns a part's decoded bytes, from the separate
// attachments.get call Gmail requires for anything sizeable and from the
// inline data field for the small parts it returns in the message itself.
//
// Base64 here is strict: unlike a body, where a garbled tail still reads as
// text, a partially decoded binary file is corrupt in ways nothing downstream
// can detect.
func attachmentData(ctx context.Context, svc *gmail.Service, id string, part *gmail.MessagePart) ([]byte, error) {
	encoded := part.Body.Data
	if part.Body.AttachmentId != "" {
		body, err := svc.Users.Messages.Attachments.
			Get(meUser, id, part.Body.AttachmentId).Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("fetching part %q of message %q: %w", part.PartId, id, err)
		}
		encoded = body.Data
	}

	data, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(encoded)
	if err != nil {
		// Gmail is inconsistent about padding across endpoints.
		data, err = base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decoding part %q of message %q: %w", part.PartId, id, err)
		}
	}
	return data, nil
}

func checkAttachmentSize(size int64, maxBytes int, partID, id string) error {
	if maxBytes == NoMaxBytes || size <= int64(maxBytes) {
		return nil
	}
	return fmt.Errorf("part %q of message %q is %d bytes against a cap of %d: %w "+
		"(re-run with a higher --max-bytes, or --max-bytes 0 for no cap; "+
		"`mail read` reports every attachment's size, so a caller can skip it without asking for it)",
		partID, id, size, maxBytes, ErrAttachmentTooLarge)
}

// findPart returns the part with the given part id, at any nesting depth.
//
// Selection is by part id alone, with no filename requirement: an inline
// image referenced by a cid: URL is content, and some senders attach one with
// no filename at all. Refusing those would leave exactly the parts an html
// body depends on unfetchable.
func findPart(part *gmail.MessagePart, partID string) *gmail.MessagePart {
	if part == nil {
		return nil
	}
	if part.PartId == partID {
		return part
	}
	for _, child := range part.Parts {
		if found := findPart(child, partID); found != nil {
			return found
		}
	}
	return nil
}

// attachmentPartIDs lists the part ids that carry fetchable content, for the
// error a caller reads when the id it had does not appear in the message.
// Naming the alternatives is the difference between a caller correcting its
// call and a caller retrying the same one.
func attachmentPartIDs(part *gmail.MessagePart) []string {
	if part == nil {
		return nil
	}
	var ids []string
	if part.Body != nil && part.Body.AttachmentId != "" {
		ids = append(ids, part.PartId)
	}
	for _, child := range part.Parts {
		ids = append(ids, attachmentPartIDs(child)...)
	}
	return ids
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory and a rename, so an interrupted fetch leaves no short file behind
// for a later pass to mistake for a complete one. A bulk fetcher that resumes
// has no way to tell the two apart otherwise.
//
// The mode is 0600: the bytes are private mail, and a corpus directory of
// them should not be world-readable because one attachment happened to be
// written by a process with a permissive umask. path is the caller's own,
// never a filename from the message, so nothing here can be steered by a
// sender.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".docket-attachment-*")
	if err != nil {
		return fmt.Errorf("%w: creating a temporary file in %q: %v "+
			"(the directory must exist and be writable; docket does not create it)", ErrOutputWrite, dir, err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("%w: setting permissions on %q: %v", ErrOutputWrite, tmp.Name(), err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("%w: writing to %q: %v", ErrOutputWrite, tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: closing %q: %v", ErrOutputWrite, tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("%w: moving the attachment into place at %q: %v", ErrOutputWrite, path, err)
	}
	return nil
}
