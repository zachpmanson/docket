# docket

An agent-facing Gmail and Google Calendar CLI. A single Go binary exposes mail and
calendar as composable subcommands with **structured output**, designed to be driven by
an LLM agent on a headless server — not clicked by a human in a terminal.

```
docket <auth|mail|cal> <subcommand> [flags]
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
3. **Reads are cheap and safe; writes are explicit.** Every mutating command requires
   `--confirm` and supports `--dry-run`.
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

## Setup

Auth is **configuration, not code**: the OAuth provider lives in a config file, so the
provider's borrowed client (`config/config.toml` ships in-repo) can be swapped for your own
registered client without touching code.

```bash
# 1. Provision config (already in repo — copy or point XDG_CONFIG_HOME at it)
mkdir -p ~/.config/docket
cp config/config.toml ~/.config/docket/config.toml

# 2. Login. On a headless box, docket prints the SSH tunnel to run locally:
docket auth login
#    ssh -L 8080:localhost:8080 server   # then open the printed URL in a browser
```

Login runs a loopback listener and completes the PKCE/OAuth flow; the token is stored at
`$XDG_STATE_HOME/docket/token.json` (`0600`). You can also run the flow on a laptop and pipe
the token to the server:

```bash
docket auth export > token.json          # from the laptop
cat token.json | docket auth import      # on the server
```

Verify with `docket auth whoami`.

> **Warning:** the borrowed OAuth client is Mozilla Thunderbird's — it can vanish when
> Thunderbird moves to dynamic client registration, and your Google security page will
> attribute docket's access to *Mozilla Thunderbird*. For anything beyond personal use,
> register your own client and point `config.toml` at it (see `docket-design.md` §8).

---

## Commands

### `docket auth`
| Subcommand | Purpose |
|---|---|
| `login` | Start OAuth login (prints tunnel + URL) |
| `whoami` | Resolve the authenticated account address |
| `export` / `import` | Pipe a token between machines (refresh tokens aren't machine-bound) |

### `docket mail` — Gmail (REST API)
| Subcommand | Purpose |
|---|---|
| `mail search --query "..."` | Native Gmail query syntax (`from:emiel is:unread`) |
| `mail list --label INBOX [--unread]` | List messages in a label |
| `mail read --id <gm-msgid>` | Full body; prefers `text/plain`, truncates at `--max-bytes` (`0` = no cap); `--html` adds the raw `text/html` part |
| `mail thread --id <gm-thrid>` | Read a whole conversation (envelopes only unless `--html`) |
| `mail send --to ... --subject ... --body-file -` | Send (mutating: `--confirm`) |
| `mail reply --id <gm-msgid> --body-file -` | Reply (mutating: `--confirm`) |
| `mail label --id <gm-msgid> --add Foo --remove INBOX` | Apply/remove labels (mutating: `--confirm`) |

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
  not: inline images appear under `attachments` when they carry a filename, and the `cid:`
  URLs in the markup that point at them do not resolve to anything docket hands back.
  A message with only a `text/html` part still gets a `body`, its markup rendered as text.
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
- `--verbose` adds the threading/cc columns to the *terminal* table. JSON output always
  contains every field, so it does nothing for a programmatic caller. `--all` is a
  deprecated alias.

### `docket cal` — Google Calendar (CalDAV + client-side RRULE expansion)
| Subcommand | Purpose |
|---|---|
| `cal agenda [--days 7]` | Upcoming events |
| `cal show --id <event-id>` | One event (id form `<uid>::<RFC3339 start>`) |
| `cal freebusy --start ... --end ...` | Busy ranges in a window |
| `cal find-slot --duration 45m --within 5d --hours 09:00-17:00` | **Find free time** — the highest-value command |
| `cal create --summary ... --start ... --duration ...` | Create (mutating: `--confirm`) |
| `cal update --id <event-id> ...` | Update (mutating: `--confirm`) |
| `cal delete --id <event-id>` | Delete (mutating: `--confirm`) |

Notes:

- **`--tz` is required** on every time-parsing command (IANA zone, e.g.
  `Australia/Melbourne`). Google's CalDAV interface can't cheaply report a calendar's
  configured timezone, and guessing causes silent off-by-hours bugs.
- `cal create` accepts `--rrule` as a raw RFC 5545 value (`"FREQ=WEEKLY;BYDAY=MO,WE,FR"`),
  plus optional `--attendees`, `--location`, and `--idempotency-key` (reusing a key on retry
  prevents duplicate events).
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
| 6 | Refused: mutating command without `--confirm`, or writes disabled |

Code 6 is the interesting one: it lets an agent *propose* a write, get a structured refusal
describing exactly what would have happened, and surface that for approval — rather than
acting unilaterally.

---

## Operator controls

Writes can be shut off (or scoped) without touching agent-facing flags or redeploying —
handy for a systemd unit's `Environment=`. Any non-empty value counts as set.

| Variable | Effect |
|---|---|
| `DOCKET_READONLY` | Disables all mail and calendar writes |
| `DOCKET_MAIL_READONLY` | Disables `mail send`/`reply`/`label` only |
| `DOCKET_CAL_READONLY` | Disables `cal create`/`update`/`delete` only |
| `DOCKET_CAL_OWN_EVENTS_ONLY` | `cal update`/`delete` refuse anything docket didn't create (via a `[docket]` marker in the event description) — a middle tier between read-only and full write |

These combine into three deployment tiers: fully open, own-events-only, fully closed.
A refusal here is `WRITES_DISABLED` / `NOT_OWNED`, exit code 6 — **refused, not failed.**

---

## Layout

```
cmd/docket/main.go     entrypoint, command registry, flag parsing, write gate
internal/
  auth/                provider config, PKCE login flow, flock'd token store
  mail/                Gmail REST v1 wrapper, MIME part walking, labels
  cal/                 CalDAV client, RRULE expansion, derived free/busy, find-slot
  out/                 result envelope, error codes, TTY detection
config/config.toml     OAuth provider + default_calendar
flake.nix              devshell: Go toolchain, gopls, golangci-lint
docket-design.md       design rationale, known limitations, build history
```

---

## Design & limitations

For the gory details — why CalDAV over the REST API, the client-side RRULE expansion gaps,
the flock-guarded token refresh, the write-safety gate, and known risks (the borrowed
client, PKCE/redirect drift, blast radius) — read [`docket-design.md`](docket-design.md).
It's the source of truth; this README is the elevator pitch.
