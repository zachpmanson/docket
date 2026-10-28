# docket

An agent-facing Gmail and Google Calendar CLI. A single Go binary exposes mail and
calendar as composable subcommands with **structured output**, designed to be driven by
an LLM agent on a headless server — not clicked by a human in a terminal.

```
docket [--account <name>] <auth|mail|cal> <subcommand> [flags]
```

The full design rationale, known limitations, and operator controls live in
[`docket-design.md`](docket-design.md). This README covers what it does and how to run it.

---

## Why it exists

An agent tool needs to be scriptable end-to-end: no interactive prompts, stable IDs
instead of positions, cheap reads and explicit writes, and errors that tell the model
exactly what to call next. `docket` bakes those rules into the binary so no external
policy layer has to.

**Design principles** (see `docket-design.md` §1):

1. **Structured by default.** JSON when stdout isn't a TTY; a human-readable table when it is.
2. **Stable IDs, never positions.** `mail read`/`cal show` address messages and events by
   their stable Gmail/Calendar IDs, not by index or sequence number.
3. **Reads and writes are permission-gated per account.** Authorized mail/calendar
   mutations also require `--confirm` and support `--dry-run`.
4. **No interactive prompts, ever.** Anything that would block on stdin is a failure with a
   distinct exit code.
5. **Errors are data.** Machine-readable error codes with a `retryable` flag, and messages
   written for the *model caller* — they say what was wrong with *this* invocation and what a
   corrected call looks like.
6. **Self-describing.** The command registry is the single source of tool definitions, so the
   agent's tool list never drifts from the binary.

---

## Install / build

```bash
# Nix (devshell with Go toolchain, gopls, golangci-lint)
nix develop

# or, plain Go
go build -o docket ./cmd/docket

# Makefile targets: build, format, test, vet
make build
```

## Nix dependency checks and CI

```bash
nix develop --command make test vet  # root and nested gmail module
nix flake check
nix build .#default.goModules  # realise first: --rebuild requires an existing output
nix build .#default.goModules --rebuild
nix build .#default
```

The `CI` workflow runs these checks on every pull request and push to `master`.
**Require the `Nix CI` check in branch protection/rulesets** to make it a merge
gate (repository configuration, not managed by this flake).

The forced `goModules` rebuild is intentional: a normal package build can reuse a
fixed-output dependency tree already cached under an outdated `vendorHash`.
`--rebuild` regenerates the tree even when cached and rejects a hash mismatch.

`overrideModAttrs` removes only the local `gmail/` replacement from the vendored
dependency output, leaving `modules.txt` and external dependencies hash-checked.
`postConfigure` then copies the current `gmail/` source into the package build.
Local implementation-only edits therefore do not need a new `vendorHash`;
dependency or module-metadata changes still require the correct hash. After such
changes, force the rebuild above and update `vendorHash` to the reported hash.

## Setup

Auth is **configuration, not code**: the OAuth provider lives in a config file, so the
provider's borrowed client (`gmail/config/config.toml` ships in-repo) can be swapped for your own
registered client without touching code.

```bash
# 1. Provision config (already in repo — copy or point XDG_CONFIG_HOME at it)
mkdir -p ~/.config/docket
cp gmail/config/config.toml ~/.config/docket/config.toml

# 2. A human must explicitly grant auth.login (and auth.import/export if needed)
# in permissions.accounts.default in config.toml. Credentials are denied by default.
# See permissions.md; ordinary restricted agent environments cannot enable token management.

# 3. Login. On a headless box, docket prints the SSH tunnel to run locally:
docket auth login
#    ssh -L 8080:localhost:8080 server   # then open the printed URL in a browser
```

Login binds only to `127.0.0.1`, using port `8080` by default. If that port is occupied,
choose another with `docket auth login --account secondary --port 8081`
(or `docket auth add --account secondary --port 8081`). `--port` accepts `1`–`65535`;
the OAuth redirect and printed SSH tunnel both use that port, e.g.
`ssh -L 8081:localhost:8081 server`. The selected account still requires an explicit
`auth.login` grant, and all existing read-only/auth-management restrictions apply.

Login completes the PKCE/OAuth flow; the default token is stored at
`$XDG_STATE_HOME/docket/accounts/default/token.json` (`0600`). If an older
`$XDG_STATE_HOME/docket/token.json` exists, Docket copies it to the default profile and keeps
the original file. You can also run the flow on a laptop and pipe
the token to the server:

```bash
docket auth export > token.json          # from the laptop
cat token.json | docket auth import      # on the server
```

Verify with `docket auth whoami`.

### Multiple accounts

The existing token at `$XDG_STATE_HOME/docket/token.json` migrates to the `default` profile
when first used. Docket keeps the original file during migration. Profiles use separate files
under `$XDG_STATE_HOME/docket/accounts/<name>/token.json`.
List profiles and their verified email/status with `docket auth list` (or `docket auth accounts`);
create a profile with `docket auth add --account work` or `docket auth login --account work`
(or import a token with `docket auth import --account work`). Remove a profile with
`docket auth remove --account work`.

Pass `--account <name>` to any account-scoped auth, mail, or calendar command. With zero
profiles, commands use `default` and report a missing token if none exists. With one configured
profile, commands select it. With multiple profiles, commands require `--account`; selection
uses the configured profile list and does not fall back if its token is invalid or unavailable.
For example: `docket --account work mail search --query "is:unread"`.

Tokens are private credentials. Keep exports secure and delete them after import.

### Trusted agent auth context

A deployment may opt into `DOCKET_AGENT_AUTH=1` **only in a trusted, auth-only
runner** allowing exactly `auth login`, `auth add`, and `auth remove`. This is an
additional restriction, not a permission grant: immutable policy must explicitly
grant `auth.login` / `auth.remove`, and all existing read-only/owned-event ceilings
still win. Keep the normal mail/calendar runner and its ceilings unchanged. The
runner must pin HOME, XDG paths, provider/config and this guard outside the agent's
control; use the **same config directory and writable account registry** for both
contexts. Do not expose the raw credential-owning UID or a general command runner.

In this context, login/add requires an explicit **new** `--account` profile name;
`default` is reserved. Registered names, existing profile directories (even empty
ones), symlinks and invalid metadata are refused before OAuth. `--account` is a
local label, **not an email selector**: a human chooses the Google identity and
consents in the browser. Two different labels can represent the same identity.
Consent tokens are installed atomically without replacing any existing token,
including when another login completes during consent. No token is printed.

Removal requires `docket auth remove --account <name> --confirm`; omitting
confirmation changes nothing. It deletes local credentials/registry metadata,
not Google's authorization, and does not cancel already running API calls.
Import/export are always refused under this guard. Normal human CLI behavior
without the guard still permits authorized reauthentication and removal without
`--confirm`.

A failure after token installation preserves the token and reports human recovery
required; do not overwrite or blindly retry. A human should inspect local state
and reconcile the registry (or deliberately remove the profile) before retrying.
The no-clobber guarantee covers cooperating guarded flows, not root or a human
with direct credential write access. Trusted deployment details: [permissions.md](permissions.md#trusted-agent-auth-guard).

> **Warning:** the borrowed OAuth client is Mozilla Thunderbird's — it can vanish when
> Thunderbird moves to dynamic client registration, and your Google security page will
> attribute docket's access to *Mozilla Thunderbird*. For anything beyond personal use,
> register your own client and point `config.toml` at it (see `docket-design.md` §8).

---

## Commands

### `docket auth`
| Subcommand | Purpose |
|---|---|
| `login` / `add --account <name>` | Start OAuth login (prints tunnel + URL) |
| `whoami` | Resolve the authenticated account address |
| `list` / `accounts` | List profiles, verified email addresses, and token status |
| `remove --account <name>` | Remove a profile token |
| `export` / `import` | Pipe a token between machines (refresh tokens aren't machine-bound); accept `--account` |

### `docket mail` — Gmail (REST API)
| Subcommand | Purpose |
|---|---|
| `mail search --query "..."` | Native Gmail query syntax (`from:emiel is:unread`) |
| `mail list --label INBOX [--unread]` | List messages in a label |
| `mail read --id <gm-msgid>` | Full body; prefers `text/plain`, truncates at `--max-bytes` (`0` = no cap); `--html` adds the raw `text/html` part |
| `mail thread --id <gm-thrid>` | Read a whole conversation (envelopes only unless `--html`) |
| `mail attachment --id <gm-msgid> --part <part-id> --out <path>` | Write one attachment's bytes to a file |
| `mail send --to ... --subject ... --body-file -` | Send (mutating: `--confirm`) |
| `mail reply --id <gm-msgid> --body-file -` | Reply (mutating: `--confirm`); `--reply-all` answers everyone the message was addressed to |
| `mail label --id <gm-msgid> --add Foo --remove INBOX` | Apply/remove labels (mutating: `--confirm`) |

The CLI's `send`/`reply` build a `text/plain` message; the library they call (`gmail/mail`)
takes a `Body` — the text and, when a caller has one, the same words as HTML — and writes the
second as a part of the same `multipart/alternative`. See
[docket-design.md](docket-design.md#what-a-message-docket-sends-is-made-of).

`search`/`list` return envelopes only (id, thread id, from/to, subject, date, labels,
snippet) — bodies are expensive, so callers ask for them explicitly with `read`.

Notes:

- `--limit` is the size of **one page**, defaulting to 25 and capped at 500 (Gmail's own
  ceiling for a single `messages.list` call). Asking for more is a usage error rather than a
  silent clamp, because 500 results out of 3000 are indistinguishable from a complete answer.
- To walk a result set larger than one page, pass the `page.next_page_token` from the
  envelope back as `--page-token`. `page.has_more` is the short answer to "did I get
  everything"; see the output contract below.
- `mail read --max-bytes 0` returns the whole body. Truncation cuts from the **end**, which
  in a forwarded trail is the oldest quoted material, so the cap is worth turning off
  whenever the point of the read is history rather than the latest reply. `truncated: true`
  says a cap was applied.
- `--html` on `read` adds two fields: `body_html`, the `text/html` part exactly as sent,
  and `html_status`, which is `present` or `none`. `none` means the message genuinely has no
  html part — a text-only message is normal mail, and a caller has to be able to tell that
  from a flag that did nothing, which is why the field is absent altogether without `--html`.
  Nothing is sanitised: strip what you will not render at render time, on the reasoning that
  you can inspect your own sanitiser and cannot inspect ours.
- Which part `--html` returns: the first `text/html` part in the MIME tree, and `body` stays
  the first `text/plain` part. For the usual `multipart/alternative` that is the two
  representations of the same message. Nesting does not change the choice — a
  `multipart/mixed` wrapping an alternative resolves to the same pair. In a
  `multipart/related`, the html part is returned and the `image/*` parts it references are
  not: inline images appear under `attachments` when they carry a filename, and a `cid:` URL
  in the markup is resolved by matching it to that entry's `content_id` and fetching the part
  with `mail attachment`. A message with only a `text/html` part still gets a `body`, its markup rendered as text.
- `--html` on `thread` returns every message's `body` and `body_html` rather than envelopes
  alone, from the same single API call, and honours `--max-bytes` per body. It is opt-in
  because a conversation's worth of bodies is the largest response docket produces. The
  text body comes along with the markup because a text-only reply in the middle of a thread
  has nothing else to render.
- `html_truncated: true` says the markup you hold is incomplete. It is separate from
  `truncated`, which is about `body`: the cap applies to each body on its own, and an html
  part is routinely an order of magnitude larger than the text beside it, so one flag for
  both would report a whole text body as cut. Truncated markup is cut back to sit after the
  last complete tag and character reference, so it parses — a cut left mid-tag does not, a
  parser swallows the remainder of the document into an attribute value. Elements left
  unclosed are fine; every html parser closes those implicitly.
- `mail attachment` writes the bytes to `--out` and returns the path, size,
  mime type, filename and a sha256 of the content in the envelope. The bytes do not go to
  stdout: stdout carries the JSON envelope, and a caller that has to locate the envelope
  inside a stream of PNG bytes cannot read a failure at all — which is the one thing it must
  never lose. Nothing is resized, re-encoded or sniffed; the file holds what the sender sent.
- One part per invocation. Gmail has no batch attachments endpoint, so several parts in one
  call would save a subprocess spawn and not one round trip, and the result would need a
  per-part shape that the envelope's single `ok` cannot express — a call where three of five
  parts are gone is neither a success nor a failure.
- The destination directory must already exist; docket does not create it, because a typo'd
  path silently growing a directory tree is worse than a failed fetch. The write goes through
  a temporary file and a rename, mode `0600`, so an interrupted fetch leaves no short file
  for a later pass to mistake for a complete one, and a corpus of private mail is not left
  world-readable by whatever umask happened to be in force. An existing file at `--out` is
  overwritten: a resumed backfill re-fetches what it already has. A final symlink is
  replaced, not followed; its target is unchanged.
- Operators can set `DOCKET_ATTACHMENT_ROOT` to an absolute, dedicated output directory.
  Relative `--out` paths then resolve beneath it; absolute paths must also be inside it.
  Parent traversal and symlink escapes are blocked with Go's `os.Root`, with the parent
  pinned before credentials or Gmail access. The root itself cannot be replaced. Keep
  config, tokens, registry/state and other sensitive files outside this tree.
  If any deployment restriction (`DOCKET_READONLY`, `DOCKET_MAIL_READONLY`,
  `DOCKET_CAL_READONLY`, `DOCKET_CAL_OWN_EVENTS_ONLY`) is nonempty and no root is set,
  attachment output fails closed with `OUTPUT_WRITE_FAILED`. Unrestricted trusted
  same-UID CLI use without a root retains arbitrary output paths; it is not a sandbox.
  Files remain private to the writing UID: a privileged runner's caller is **not**
  automatically granted read access. See [operator rollout guidance](permissions.md#attachment-output-boundary).
- `--max-bytes` on `attachment` means the same as everywhere else, `0` included, and defaults
  to 10 MB. Over the cap is `ATTACHMENT_TOO_LARGE`, not a truncated file — half a PNG is not
  a smaller PNG, and once written to disk it is indistinguishable from a whole one. The cap
  is checked against the size in the message metadata *before* the content call, so an
  oversized attachment costs one cheap request rather than a download; it is checked again
  against the bytes received, because the metadata size is Gmail's claim and not a
  measurement. `mail read` reports every attachment's `size`, so a caller can skip one
  without asking for it at all; the cap is there for the accident.
- The four ways a fetch can come back empty stay four codes, because a caller walking
  months-old metadata acts differently on each: `MESSAGE_NOT_FOUND` (exit 4, the message is
  gone), `PART_NOT_FOUND` (exit 4, the message is there and has no such part — the error
  names the part ids it does have), `ATTACHMENT_UNAVAILABLE` (exit 4, the part is there with
  no content behind it), and `OUTPUT_WRITE_FAILED` (exit 1, the destination was refused or the local
  write failed — nothing about the mailbox is wrong). A rate limit stays `RATE_LIMITED`,
  exit 5, `retryable: true`: reported as a missing part it would be recorded as a permanent
  gap and never asked for again.
- `attachments` entries carry `content_id` for the parts an html body references by
  `cid:`, with the angle brackets stripped so it matches the URL token directly. Without it
  a consumer holding `cid:ii-9f3c2a@mail.example.com` has no way to say which part id that
  is, and an inline screenshot — content, not decoration — stays a broken image.
- `--verbose` adds the threading/cc columns to the *terminal* table. JSON output always
  contains every field, so it does nothing for a programmatic caller. `--all` is a
  deprecated alias.

### `docket cal` — Google Calendar (CalDAV + client-side RRULE expansion)
| Subcommand | Purpose |
|---|---|
| `cal list` | List calendar IDs and display names for the selected account (no `--tz` needed) |
| `cal agenda [--days 7]` | Upcoming events |
| `cal show --id <event-id>` | One event (id form `<uid>::<RFC3339 start>`) |
| `cal freebusy --start ... --end ...` | Busy ranges in a window |
| `cal find-slot --duration 45m --within 5d --hours 09:00-17:00` | **Find free time** — the highest-value command |
| `cal create --summary ... --start ... --duration ...` | Create a timed event (mutating: `--confirm`) |
| `cal create --summary ... --start YYYY-MM-DD --end YYYY-MM-DD --all-day` | Create an all-day event (mutating: `--confirm`; end date is exclusive) |
| `cal update --id <event-id> ...` | Update (mutating: `--confirm`) |
| `cal delete --id <event-id>` | Delete (mutating: `--confirm`) |

Notes:

- `docket --account default cal list` discovers the account's CalDAV calendars and
  returns `data: [{"id":"...","name":"..."}]` in the standard JSON envelope
  (ID/NAME columns on a terminal). Use the exact `id`, not the display name, with
  `--calendar <id>` to select a destination. It lists collections only, not events;
  an empty result is `data: []`. Shared/read-only and virtual calendars may appear:
  discovery does not imply permission to write. Virtual calendars remain excluded
  from `--calendar all` event queries. No timezone or confirmation is needed.
- **`--tz` is required** on every time-parsing command (IANA zone, e.g.
  `Australia/Melbourne`). Google's CalDAV interface can't cheaply report a calendar's
  configured timezone, and guessing causes silent off-by-hours bugs.
- `cal create` accepts `--rrule` as a raw RFC 5545 value (`"FREQ=WEEKLY;BYDAY=MO,WE,FR"`),
  plus optional `--attendees`, `--location`, and `--idempotency-key` (reusing a key on retry
  prevents duplicate events). For all-day events, pass `--all-day`, date-only `--start` and
  `--end` values (`YYYY-MM-DD`), and no `--duration`; `--end` is exclusive per calendar
  convention (for a one-day event, the end date is the following day).
- Calendar selection: `--calendar` defaults to the account's primary calendar for
  reads, and to `default_calendar` in `config.toml` for writes. `agenda`/`freebusy`/
  `find-slot` accept `--calendar all` to merge every calendar (so busy slots respect
  holidays/shared/work calendars).
- A **brand-new** event may not appear in `agenda`/`freebusy` for minutes via the legacy
  CalDAV time-range query — trust `create`'s own return value (or `cal show` on the returned
  id) rather than immediately re-querying `agenda` to confirm.
- Known gap: `cal update --location ""` can't blank a field (empty == "not provided").

---

## Output contract

Every command emits **one JSON envelope** on stdout:

```json
{ "ok": true, "data": { }, "warnings": [], "error": null }
```

`search`/`list` add a `page` object describing what you are holding:

```json
{ "ok": true, "data": [ ],
  "page": { "returned": 500, "limit": 500, "has_more": true, "next_page_token": "09vv…" },
  "error": null }
```

```json
{ "ok": false, "data": null,
  "error": { "code": "AUTH_EXPIRED", "message": "refresh token rejected", "retryable": true } }
```

Two invariants hold for every command, enforced where the envelope is serialised:

- `ok: false` always carries a non-null `error` with a populated `code`, `message` and
  `retryable`, and always exits non-zero. A caller reading `ok` and then `error.message`
  never has to handle a null.
- `retryable: true` is claimed only for causes known to be transient — rate limits, 5xx,
  network timeouts, a refresh worth reattempting. A usage error, a missing message, or an
  unrecognised failure reports `false`, so a client that backs off on `retryable` never
  loops on something that cannot succeed.

Mail error codes a caller may branch on: `RATE_LIMITED`, `AUTH_EXPIRED`, `AUTH_REVOKED`,
`PERMISSION_DENIED`, `MESSAGE_NOT_FOUND` / `THREAD_NOT_FOUND`, `GMAIL_SERVER_ERROR`,
`NETWORK_ERROR`, `TIMEOUT`, `USAGE_ERROR`, `SEND_FAILED`, `CONFIRM_REQUIRED`,
`WRITES_DISABLED`, `GMAIL_API_ERROR` (unclassified).

**Exit codes** (an agent's control flow is driven by these, not by parsing prose):

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Everything else |
| 2 | Usage error — malformed invocation (message names the fix) |
| 3 | Auth required / expired — needs a human |
| 4 | Not found |
| 5 | Rate limited or transient — safe to retry with backoff |
| 6 | Refused: operation permission denied, missing `--confirm`, or legacy write/ownership restriction |

Code 6 is the interesting one: it lets an agent *propose* a write, get a structured refusal
describing exactly what would have happened, and surface that for approval — rather than
acting unilaterally.

---

## Operator controls

See **[Per-account operation permissions](permissions.md)** for the complete validated
TOML schema, every read/write/auth operation, inheritance, account selection and safe
Nix rollout instructions. **Upgrade note:** omitted mail/calendar write and credential
permissions now deny; existing reads remain allowed. Existing TOML is never rewritten,
and `config.json` is not read. `accounts.json` remains metadata, not policy.

For example (configuration illustration, **not a live permission grant**):

```toml
# Above all table headers:
read_only_accounts = ["work"]

# After your existing provider settings:
[permissions.accounts.default]
"mail.label" = true
"mail.send" = false
"mail.reply" = false
```

This allows personal mail reads + receipt labels while leaving work read-only, **only
if the deployment's mail kill switch is not set**. Permissions never override the
privileged runner, read-only ceilings, confirmation or owned-event restrictions.

Writes can be shut off (or scoped) without touching agent-facing flags or redeploying —
handy for a systemd unit's `Environment=`. Any non-empty value counts as set.

| Variable | Effect |
|---|---|
| `DOCKET_ATTACHMENT_ROOT` | Absolute, dedicated attachment output directory; required in restricted deployments (see above) |
| `DOCKET_READONLY` | Disables all mail and calendar writes |
| `DOCKET_MAIL_READONLY` | Disables `mail send`/`reply`/`label` only |
| `DOCKET_CAL_READONLY` | Disables `cal create`/`update`/`delete` only |
| `read_only_accounts` in `~/.config/docket/config.toml` | Disables all writes for each listed account name |
| `DOCKET_CAL_OWN_EVENTS_ONLY` | `cal update`/`delete` refuse anything docket didn't create (via a `[docket]` marker in the event description) — a middle tier between read-only and full write |

Credential login/import/export/removal also require explicit grants and are blocked by
any read-only environment flag, the selected account's read-only ceiling, or the
owned-events-only flag: credentials must not be an escape hatch. The privileged
runner's `auth whoami`-only allowlist remains a separate boundary.

Global and surface-specific environment controls remain deployment-wide. `read_only_accounts`
provides per-account read-only access, for example `read_only_accounts = ["archive"]`.
These controls combine into three deployment tiers: fully open, own-events-only, fully closed.
A refusal here is `WRITES_DISABLED` / `NOT_OWNED`, exit code 6 — **refused, not failed.**

---

## Layout

```
cmd/docket/main.go     thin CLI shell over the gmail library
 gmail/                  reusable library module (github.com/zachpmanson/docket/gmail)
  config/              default config.toml embedded into the auth package
  auth/                provider config, PKCE login flow, flock'd token store
  mail/                Gmail REST v1 wrapper, MIME part walking, labels
  cal/                 CalDAV client, RRULE expansion, derived free/busy, find-slot
  errors/              shared process exit codes
internal/
  out/                 result envelope, TTY detection
flake.nix              devshell: Go toolchain, gopls, golangci-lint
docket-design.md       design rationale, known limitations, build history
```

---

## Design & limitations

For the gory details — why CalDAV over the REST API, the client-side RRULE expansion gaps,
the flock-guarded token refresh, the write-safety gate, and known risks (the borrowed
client, PKCE/redirect drift, blast radius) — read [`docket-design.md`](docket-design.md).
It's the source of truth; this README is the elevator pitch.
