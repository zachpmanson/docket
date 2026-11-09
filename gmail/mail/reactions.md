# Received-reaction evidence (inspection v2)

`Message.ReactionInspection` and full (`IncludeHTML=true`) `ThreadMessage.ReactionInspection`
are additive `reaction_inspection` JSON sidecars. `Read` always inspects the existing
Gmail `format=full` result, independently of HTML/body display caps. Metadata-only
threads omit the field. Nil/missing (including old decoder JSON) means **not
inspected**, never inspected-none. Existing envelope threading fields, first
text/HTML selection, attachment metadata and rendering/capping are unchanged.
The inspection walker is separate from that legacy body decoder.

## Exported contract

- `ReactionInspection`: `Version` (currently `ReactionInspectionVersion=2`),
  `EmojiVersion`, `Status`, `Layout`, repeated raw `InReplyTo` values,
  `TargetMessageID`, `Candidates`, `Fallbacks`, `Reasons`.
- `ReactionFallback`: direct alternative `PartPath`, `MIMEType`, `Charset`,
  `DecodedBytes` and `BodyComplete`. No fallback text is copied into this sidecar.
- `ValidateReactionInspection(*ReactionInspection) error` revalidates the
  versioned contract, target/header consistency, strict layout evidence, original
  candidate JSON and exact Unicode sequence. Consumers should call it before
  classifying a deserialized inspection; nil/unknown/inconsistent evidence fails
  closed. Version 1 cannot establish fallback completeness and is rejected for
  classification. Candidate and fallback paths must cover all direct alternative
  positions without gaps. The Gmail payload version remains 1. This evidence
  contract does not authenticate an arbitrary serialized assertion.
- `ReactionCandidate`: adapter `Format`, stable tree `PartPath` (root `0`,
  zero-indexed children `0.0`, ...), provider `PartID`, parsed `MIMEType`, `Charset`,
  `Disposition`, original `ContentTypeHeaders`/`DispositionHeaders`, external
  `AttachmentID`, decoded `Body`, `BodyComplete`, payload `Version`, exact `Emoji`,
  `Status` and diagnostic `Reason`.
- Status strings/constants: `none`/`ReactionNone`,
  `candidate`/`ReactionStatusCandidate`, `unsupported`/`ReactionUnsupported`,
  `malformed`/`ReactionMalformed`, `incomplete`/`ReactionIncomplete`,
  `ambiguous`/`ReactionAmbiguous`.

Example (other evidence fields omitted here):

```json
{"reaction_inspection":{"version":2,"emoji_version":"Unicode Emoji 16.0 (fully-qualified)","status":"candidate","layout":"top-level","in_reply_to":["<target@example.test>"],"target_message_id":"<target@example.test>","candidates":[{"format":"gmail-email-reaction/v1","part_path":"0","mime_type":"text/vnd.google.email-reaction+json","body":"{\"version\":1,\"emoji\":\"❤️\"}","body_complete":true,"version":1,"emoji":"❤️","status":"candidate"}]}}
```

`candidate` means validated sender-supplied structural evidence, **not** an
instruction to hide a source message, a resolved target, authenticated sender
intent or provider lifecycle state. From/Date/source IDs remain in the envelope;
account access, source provenance, association, dedupe, storage, UI, counts and
legacy backfill belong to the consuming application. No reaction-text heuristic
is used. The owner's observed decoded heart fallback has **not** been verified
as this MIME format; all tests are synthetic.

## Adapter and conservative policies

The initial adapter `GmailReactionFormat="gmail-email-reaction/v1"` implements
[Google's documented MIME format](https://developers.google.com/workspace/gmail/reactions/format):
`text/vnd.google.email-reaction+json`, JSON object, integer `version: 1`, exactly
one supported emoji. MIME types/dispositions are parsed case-insensitively with
parameters. UTF-8 or omitted charset is supported; Gmail API base64url transport
is decoded once (padded or unpadded), not MIME transfer-decoded again.

JSON must have no trailing values or duplicate keys (including escaped spellings
and nested extension objects). Harmless unknown members are accepted, never
interpreted. Malformed JSON/UTF-8/transport and invalid emoji are `malformed`;
unknown integer versions or charsets are `unsupported`. `BodyComplete` refers
only to retained bytes, not successful validation. Exact membership in the
pinned fully-qualified Unicode Emoji 16.0 table validates emoji; there is no
normalization, rune-count, general-symbol regex or grapheme-only validation.
VS16, modifiers, flags, keycaps and ZWJ sequences survive unchanged. Tradeoff:
about 45 KB source data plus an in-memory lookup map, no dependency/module/pin
changes; unqualified/minimally-qualified forms and newer sequences remain
unsupported by this validator (reported as malformed candidate payloads).
See [table provenance](emoji/README.md).

[Experimental RFC9078](https://www.rfc-editor.org/rfc/rfc9078.html) is deliberately
**unsupported and nonconsuming** here. A `Content-Disposition: reaction` marker
is recorded as an unsupported `rfc9078` candidate without decoding its body.
A Gmail JSON part bearing that disposition is ambiguous, not RFC compliance.

Only the outer message is inspected: filename-bearing parts, MIME attachments
(including multipart attachment subtrees), and `message/*` subtrees are skipped.
Duplicate candidates, even byte-identical, are ambiguous; none is selected.
Malformed/repeated/contradictory MIME headers make inspection incomplete rather
than trusting one occurrence. Incomplete traversal/evidence/payload takes
precedence over duplicate-marker ambiguity, then the single candidate status.
Association requires exactly one raw In-Reply-To occurrence containing a single
bare bracketed ASCII Message-ID (no comments or quoted local parts). Repeated
headers, even identical, multiple IDs and unsupported syntax are ambiguous;
missing IRT makes an otherwise valid candidate incomplete. No References/thread
position/prose fallback is used. Only overall `candidate` gets `TargetMessageID`.
The original repeated values remain available (unless an explicit bound makes
inspection incomplete). No target is fetched or resolved here.

`Layout` is independent of payload validity: `top-level` means a sole Gmail JSON
leaf; `alternative` means a root multipart/alternative with one direct JSON leaf
and at most one plain and one HTML leaf, no other parts, attachment dispositions,
filenames, unknown dispositions or container body. Fallback leaves must be locally
available, fully decoded UTF-8/ASCII with consistent sizes and at most 64 KiB each;
unavailable/external/malformed/oversized alternatives downgrade to `other` even
when the reaction payload itself is a valid candidate. All other layouts are `other`.
Only a valid, resolved, authorized candidate in a strict layout could qualify for
a downstream reaction-only display policy. Generic multipart/mixed, nested
alternatives, extra authored siblings and attachments stay ordinary visible mail,
even when a valid candidate can be annotated. Nothing is removed by this library.

## Fixed limits and incomplete data

Root depth 0; maximum depth 16, 128 inspected parts, 4,096 decoded bytes per
candidate, 16,384 total decoded candidate bytes, 32,768 retained MIME/header/ID
bytes, 16 occurrences per relevant header, 2,048 bytes per API MIME type.
Limits fail closed with `incomplete` and diagnostics, never `none`. Oversized
payloads are not partially parsed. Nil/empty bodies, size mismatches, or any
`body.attachmentId` mean incomplete: that API field denotes external storage,
not MIME attachment disposition. Fallbacks have a separate 64 KiB-per-leaf bound;
incomplete fallbacks remain recorded and veto strict layout without consuming
or fetching them. No new network request is made. Candidate
body bytes are retained only when valid UTF-8; invalid bytes are diagnosed rather
than silently replaced. Evidence headers may be clipped at the byte budget,
explicitly making the whole inspection incomplete. Missing old evidence requires
an explicitly authorized re-read/preview, not classification from legacy prose.
