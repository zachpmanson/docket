# `docket` — agent-facing mail & calendar CLI

A single Go binary exposing Gmail and Google Calendar as composable subcommands with
structured output, designed to be driven by an LLM agent on a headless server.

Built in Go, with a Nix devshell (`flake.nix` / `shell.nix`) providing the Go toolchain
and any lint/build tooling so the dev environment is reproducible across machines.

---

## 1. Design principles

1. **Structured by default.** JSON when stdout isn't a TTY; human-readable table when it is.
2. **Stable IDs, never positions.** An agent must be able to `read` a message it saw three
   turns ago. Gmail message IDs and Calendar event IDs are stable; IMAP sequence numbers are not.
3. **Reads are cheap and safe; writes are explicit.** Every mutating command requires
   `--confirm` and supports `--dry-run`.
4. **No interactive prompts, ever.** Anything that would block on stdin is a failure with a
   distinct exit code instead.
5. **Errors are data.** Machine-readable error codes with a `retryable` flag, so the agent
   can distinguish "back off and retry" from "stop and ask the human".
6. **Self-describing.** `docket schema` emits tool definitions generated from the same
   command registry, so the agent's tool list can never drift from the binary.
7. **Error messages are written for the LLM caller, not a human reading a terminal.** When
   an agent misuses a command — bad flag, missing `--confirm`, invalid id, unknown label —
   the `error.message` string must say exactly what was wrong with *this* invocation and
   what a corrected call looks like (valid flags/values, the exact `--confirm` incantation,
   an example id format), not just restate the error code. Treat every error path as a
   teaching moment for the model's next tool call, since there's no human in the loop to
   infer intent from a terse message.

Two bugs an audit of every command's usage/error string caught, both fixed:
- Go's `flag` package prints its own "flag provided but not defined" + per-flag usage text
  straight to stderr on every parse error, unprompted — noise duplicating (in a different,
  inconsistent format) what the JSON envelope already says. Suppressed via `fs.SetOutput
  (io.Discard)` on every `FlagSet` (see `newFlagSet` in `cmd/docket/main.go`) — the JSON
  envelope must be the *only* thing docket emits, on stdout or stderr.
- Two `--confirm` "re-run exactly as shown" hints didn't actually reproduce the invocation
  being previewed: `cal update`'s literally contained a `"..."` placeholder instead of the
  real flags (not valid shell syntax at all), and `cal create`'s silently dropped
  `--location`/`--attendees`/`--rrule`/`--idempotency-key` even when the agent had passed
  them — copying either verbatim would have created a different event than the one just
  previewed. A missing flag in a rerun hint is a correctness bug, not a cosmetic one:
  the whole point of showing it is that copying it verbatim reproduces the preview exactly.

---

## 2. Layout

```
flake.nix         devshell: Go toolchain, gofmt/golangci-lint, gopls
cmd/docket/main.go
internal/
  auth/       provider config, PKCE flow, token store
  mail/       Gmail REST v1 wrapper, MIME part walking
  cal/        CalDAV client, RRULE expansion, derived free/busy
  out/        result envelope, error codes, TTY detection
  schema/     command registry → JSON Schema / MCP tool defs
  cache/      optional SQLite envelope index (phase 4)
```

---

## 3. Auth

### Provider is configuration, not code

The single most important structural decision. The Thunderbird credentials go in a config
file, not a constant, so swapping to your own registered client later is an edit rather
than a refactor.

```go
type Provider struct {
    ClientID     string   `toml:"client_id"`
    ClientSecret string   `toml:"client_secret"`
    AuthURL      string   `toml:"auth_url"`
    TokenURL     string   `toml:"token_url"`
    RedirectURI  string   `toml:"redirect_uri"`
    Scopes       []string `toml:"scopes"`
    UsePKCE      bool     `toml:"use_pkce"`
}
```

`~/.config/docket/config.toml`:

```toml
[provider]
# Values pinned from searchfox.org/comm-central/source/mailnews/base/src/OAuth2Providers.sys.mjs
# Verify use_pkce and redirect_uri against current source — both have changed historically.
client_id     = "..."
client_secret = "..."
auth_url      = "https://accounts.google.com/o/oauth2/auth"
token_url     = "https://www.googleapis.com/oauth2/v3/token"
redirect_uri  = "http://localhost"
use_pkce      = true
scopes = [
  "https://mail.google.com/",
  "https://www.googleapis.com/auth/calendar",
  "email",
]
```

`email` is required for `auth whoami` to resolve the account address via Google's tokeninfo
endpoint — without it the token carries no identity claim and whoami's `email` field is empty.

This exact file ships in the repo at `config/config.toml` — copy it to
`~/.config/docket/config.toml` (or point `XDG_CONFIG_HOME` at a directory containing a
`docket/` subdir with it). It's safe to commit: the Thunderbird client_id/secret aren't a
secret Zach controls, already public in Mozilla's own source (see §8). A deployment that
provisions this file automatically (e.g. a NixOS host's activation script) still needs
`auth login` + `auth export`/`auth import` run separately per §3 — the config file alone
grants no account access, only the provider registration.

Ship a `--provider=custom` path that reads `client_id`/`client_secret` from env so that
migrating means setting two environment variables.

### Login on a headless box

`docket auth login` starts a loopback listener, prints the authorization URL, and prints
the exact SSH command to tunnel it:

```
ssh -L 8080:localhost:8080 server
```

Then open the URL in the laptop browser; the redirect tunnels back to the listener.

Also implement `docket auth import --token-file -` so you can run the flow on your laptop
and pipe the resulting token to the server. Refresh tokens aren't machine-bound.

Request `access_type=offline` and `prompt=consent`. Without the latter you won't get a
refresh token on re-authorization, which produces a token that mysteriously dies after an hour.

### Token store

- `$XDG_STATE_HOME/docket/token.json`, mode `0600`, parent dir `0700`.
- Optional `--token-cmd` / `--token-encrypt-cmd` pair so the file can be age- or
  pass-encrypted rather than plaintext on disk.
- **Refresh must be flock-guarded.** Several agent invocations can run concurrently; two
  simultaneous refreshes race and one token gets invalidated. Take an exclusive `flock` on
  the token file, re-read it inside the lock (another process may have just refreshed),
  refresh only if still expiring, write, unlock.
- Wrap `oauth2.ReuseTokenSource` and persist on every change:

```go
type persistingSource struct {
    src   oauth2.TokenSource
    path  string
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
    unlock := lockFile(p.path)
    defer unlock()
    if tok, ok := readIfFresh(p.path); ok {
        return tok, nil
    }
    tok, err := p.src.Token()
    if err != nil {
        return nil, err
    }
    return tok, writeToken(p.path, tok)
}
```

- On `invalid_grant`, exit with code 3 and a message naming the likely cause (password
  change invalidates mail-scoped tokens; six months unused invalidates any token). The agent
  should surface this to you rather than retry.

### Transport auth

Mail and Calendar both authenticate as plain OAuth2 Bearer tokens over HTTPS via
`oauth2.TokenSource` — no SASL needed now that mail goes through the Gmail REST API rather
than IMAP/SMTP (§4). Access tokens last an hour; `TokenSource` (§3 token store) refreshes
transparently per request.

---

## 4. Mail

**Library:** `google.golang.org/api/gmail/v1` (REST), not IMAP/SMTP.

Originally planned on `emersion/go-imap/v2` + `go-smtp` (see git history for that draft). Dropped
after implementation revealed the library has no Gmail extension support at all — no `X-GM-RAW`
search, no `X-GM-MSGID`/`X-GM-THRID`, no label objects — and its wire-protocol package
(`imapwire`) is unexported `internal`, so there's no way to layer the extension on top from
outside the module without vendoring a fork. The Gmail REST API gives all of this natively, which
is the same tradeoff already made for Calendar (§5): native support beats reimplementing
Gmail-specific behaviour on a generic protocol.

This does **not** require a new OAuth scope or re-consent: `https://mail.google.com/` (already in
`config.toml`, already granted) is Google's documented broadest Gmail API scope — the Thunderbird
client already makes REST calls under this client for Calendar today, so this is the same pattern,
not a new one.

### Gmail-specific behaviour to encode once, centrally

| Quirk | Handling |
|---|---|
| Labels are first-class objects, not folders | `users.labels.list` once, cache id ↔ name, apply/remove via `users.messages.modify` |
| Search | `users.messages.list` with `q=` accepts native Gmail query syntax directly — no extension needed |
| Deletion | `users.messages.trash` (moves to Trash label); permanent delete is a separate, far more dangerous call — don't wire it up without a very deliberate `--confirm` story |
| Identity | `id` and `threadId` from the API response are the stable ids — use these directly, no UID translation needed |
| Bodies | MIME parts arrive base64url-encoded in `payload.parts`; walk for `text/plain`, fall back to `text/html` converted to text |

### Commands

```
docket mail search  --query "from:emiel is:unread" [--limit 25] [--since 7d]
docket mail list    --label INBOX [--limit] [--unread]
docket mail read    --id <gm-msgid> [--format text|raw] [--max-bytes 20000]
docket mail thread  --id <gm-thrid>
docket mail send    --to ... --subject ... --body-file - [--confirm]
docket mail reply   --id <gm-msgid> --body-file - [--confirm]
docket mail label   --id <gm-msgid> --add Foo --remove INBOX [--confirm]
```

### Output shape for agents

`search` and `list` return envelopes only — id, thread id, from, to, subject, date, labels,
snippet. Bodies are expensive and blow the context window; make the agent ask for them.

`read` prefers `text/plain`, falls back to HTML converted to text, truncates at `--max-bytes`
with an explicit `"truncated": true` field so the agent knows it didn't see everything.
Attachments are listed as metadata (filename, mime type, size, part id) and fetched only via
an explicit `mail attachment --part`.

---

## 5. Calendar

**Use CalDAV, not the REST API — reversed from the original plan.** The REST API
(`google.golang.org/api/calendar/v3`) is disabled on Thunderbird's borrowed OAuth project
(`SERVICE_DISABLED`, confirmed live) — a Google Cloud project we don't own and can't enable
services on. Thunderbird's desktop client doesn't use the REST API for calendar sync either;
it uses CalDAV (`https://apidata.googleusercontent.com/caldav/v2/<calendar-id>/events`), a
separate legacy interface gated by its own "CalDAV API" toggle, which is enabled on that
project because that's what the client actually calls. Same client, same `auth/calendar`
scope, no new registration — confirmed working live.

The cost: CalDAV returns raw iCalendar data, so recurring events need client-side RRULE
expansion instead of the REST API's server-side `singleEvents=true`. Libraries:
`emersion/go-webdav/caldav` (protocol client), `emersion/go-ical` (ICS parsing — its
`Component.RecurrenceSet` conveniently already builds a `teambition/rrule-go` `rrule.Set`
including EXDATE/RDATE), `teambition/rrule-go` (occurrence expansion). Modified single
occurrences are handled via `RECURRENCE-ID` override matching, grouped by `UID`.

CalDAV also has **no `free-busy-query` support** (unlike REST). `freebusy` and `find-slot`
derive busy ranges themselves from expanded events: any non-cancelled event not marked
`TRANSP:TRANSPARENT` counts as busy.

**Known limitations of the client-side expansion** (acceptable for phase 2, revisit if they
bite):
- A modified occurrence is only matched against its override if the occurrence's *original*
  scheduled time falls within the query window — an instance moved from just outside the
  window to inside it (or vice versa) won't line up with its override.
- Event ids are a synthetic `<UID>::<occurrence-start-RFC3339>` composite, not a CalDAV href
  — recurring events share one href across every occurrence, so the href alone can't address
  a specific instance. `cal show` re-derives the occurrence by re-querying a window around
  the embedded timestamp.
- No cheap way to ask Google a calendar's configured timezone over CalDAV (the REST API had
  `Calendars.Get(id).TimeZone`), so `--tz` is **required** on every command that parses times,
  rather than defaulted — see Time handling below.

**A brand-new event created via `cal create` may not appear in `cal agenda`/`freebusy`/
`find-slot` for a long time (tens of minutes, observed live).** `cal show`/`cal update`/
`cal delete` all work on it immediately — this is specifically the legacy CalDAV time-range
`REPORT` query (what agenda/freebusy/find-slot run) lagging behind Google's primary calendar
store for genuinely new events, confirmed by three live tests: (1) the event shows correctly
in Google Calendar's own UI immediately; (2) an unfiltered CalDAV `REPORT` with no time-range
finds it immediately; (3) editing a field on an *existing, already-indexed* event via `cal
update` is reflected in a time-range query instantly — only brand-new event creation is
affected. An agent that creates an event and immediately re-queries `agenda` to confirm it
should instead trust `create`'s own return value (or poll `cal show` by the returned id, which
is unaffected) rather than expecting `agenda` to reflect it right away.

### Commands

```
docket cal agenda    [--days 7] [--calendar primary] --tz <zone>
docket cal show      --id <event-id> [--calendar primary] --tz <zone>
docket cal freebusy  --start ... --end ... [--calendar ...] --tz <zone>
docket cal find-slot --duration 45m --within 5d --hours 09:00-17:00 --tz Australia/Melbourne
docket cal create    --summary ... --start ... --duration ... [--attendees ...] [--rrule ...] [--confirm]
docket cal update    --id <event-id> [--start ...] [--confirm]
docket cal delete    --id <event-id> [--confirm]
```

`--rrule` takes a raw RFC 5545 recurrence rule value (e.g. `"FREQ=WEEKLY;BYDAY=MO,WE,FR"`,
`"FREQ=DAILY;COUNT=10"`) — native syntax passthrough via `teambition/rrule-go`'s
`StrToROption`, same approach as `mail search`'s `--query`, rather than a bespoke recurrence
DSL. Verified live: a created recurring event's RRULE round-trips correctly through
`cal show`'s expansion.

Known gap: `cal update --location ""` doesn't clear the location — an empty flag value is
indistinguishable from "not provided" in the current flag handling, so there's currently no
way to blank out a field once set. Low priority; revisit if it's actually needed.

`find-slot` is the highest-value command for an agent — it turns "when can I meet Christian
on Thursday" from three round-trips of reasoning over raw events into one call. Built on the
derived-busy-ranges logic above (no native freebusy endpoint to build it on, per above).

### Time handling

- All output RFC3339 with explicit offset (or date-only `YYYY-MM-DD` for all-day events).
  Never bare local times.
- Accept relative inputs (`tomorrow 2pm`, `+3d`) but echo the resolved absolute time in the
  response so the agent can verify the interpretation was right.
- `--tz` is **required**, not defaulted to the calendar's configured zone — CalDAV gives no
  cheap way to ask for it (see Known limitations above). Falling back to the server's local
  zone was rejected specifically because it causes silent off-by-hours bugs; requiring the
  flag is the honest alternative until a cheap lookup exists.

### Idempotency

Calendar API accepts a client-supplied event `id` on insert. Have `create` take an optional
`--idempotency-key`, hash it into a valid event id, and treat a 409 as success. Agents retry;
without this you get duplicate events.

### Soft writes: `DOCKET_CAL_OWN_EVENTS_ONLY`

Between fully read-only and full write access there's a useful middle ground for an agent
deployment: let it manage events it created itself, but never touch anything a human created
directly. Every event `cal create` makes gets a `[docket]` marker written into its
`DESCRIPTION`; `cal update`/`cal delete` check for that marker when
`DOCKET_CAL_OWN_EVENTS_ONLY` is set (any non-empty value) and refuse — exit code 6,
`NOT_OWNED` — if it's missing. `cal create` itself is never restricted by this (a freshly
created event is definitionally the agent's own).

A description marker rather than an id-format check (e.g. inspecting the synthetic uid's
`@docket` suffix) is deliberate: it's visible if Zach opens the event in Google Calendar's own
UI, so the ownership boundary isn't just an internal implementation detail — he can see at a
glance which events an agent made. The tradeoff: if someone manually clears an event's
description, docket stops recognizing it as its own; judged an acceptable edge case.

This combines with `DOCKET_CAL_READONLY` (checked first, and wins if both are set) to give a
deployment three tiers: fully open, own-events-only, fully closed.

---

## 6. Agent interface contract

### Envelope

```json
{
  "ok": true,
  "data": { },
  "warnings": ["..."],
  "error": null
}
```

```json
{
  "ok": false,
  "data": null,
  "error": { "code": "AUTH_EXPIRED", "message": "refresh token rejected", "retryable": false }
}
```

`error.message` is written for the agent, per principle 7 — e.g. a bad `--label` value names
the labels that do exist; a missing `--confirm` echoes the exact command with `--confirm`
appended; an unknown `--id` states the expected id format (`X-GM-MSGID` hex string, not a
sequence number).

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 2 | Usage error — malformed invocation |
| 3 | Auth required / expired — needs human |
| 4 | Not found |
| 5 | Rate limited or transient — safe to retry with backoff |
| 6 | Refused: mutating command without `--confirm` |
| 1 | Everything else |

Code 6 matters more than it looks. It lets the agent propose an action, get a structured
refusal describing exactly what would have happened, and surface that to you for approval —
rather than either acting unilaterally or having to reason about safety itself.

### Operator controls

Every mutating command checks these environment variables, in order, before even looking at
`--confirm`/`--dry-run` — a human can shut off writes without touching agent-facing flags or
redeploying anything, e.g. by editing a systemd unit's `Environment=`. Any non-empty value
counts as set.

| Variable | Effect |
|---|---|
| `DOCKET_READONLY` | Disables all mail and calendar writes. |
| `DOCKET_MAIL_READONLY` | Disables `mail send`/`reply`/`label` only. |
| `DOCKET_CAL_READONLY` | Disables `cal create`/`update`/`delete` only. |
| `DOCKET_CAL_OWN_EVENTS_ONLY` | `cal update`/`delete` refuse anything not created via `cal create` — see §5 "Soft writes". `cal create` is unaffected. |

A refusal from any of these is `WRITES_DISABLED` (or `NOT_OWNED` for the last one), exit code
6, same category as a missing `--confirm` — refused, not failed.

### Self-description

`docket schema --format json-schema|mcp` walks the command registry and emits tool
definitions. Generate them from the same struct tags the flag parser uses, so they cannot go
stale. This also gives you a clean upgrade path if you later want to expose the binary over
MCP rather than shelling out — same registry, different transport.

---

## 7. Build order

| Phase | Scope | Done when |
|---|---|---|
| 0 | Config, provider, PKCE login, flock'd token store, `auth whoami` | Token survives a week and refreshes under concurrent invocation |
| 1 | `mail search`/`list`/`read` | Agent can answer "what did Emiel send me about X" |
| 2 | `cal agenda`/`freebusy`/`find-slot` | Agent can answer "when am I free Thursday" |
| 3 | Writes behind `--confirm`: `mail send`, `mail reply`, `cal create` | Dry-run output is reviewable |
| 4 | `schema`, SQLite envelope cache with CONDSTORE incremental sync | Search is offline and fast |

Phases 1 and 2 are where nearly all the value is. Resist starting on the cache.

---

## 8. Known risks

- **The borrowed client can vanish.** Mozilla's source comment says the hardcoded
  registration disappears once Thunderbird moves to dynamic client registration. No timeline,
  but it's the declared direction. Mitigated by provider-as-config: swapping to your own
  client is a config edit plus one re-login.
- **PKCE/redirect drift.** Thunderbird's flow parameters change more often than the
  credential. Pin from searchfox, and if login breaks after working fine, check those first.
- **Blast radius.** `https://mail.google.com/` plus `auth/calendar` on a network-reachable
  box is total control of both. Encrypt the token at rest, keep the file `0600` under a
  dedicated service user, and when you register your own client, narrow to
  `calendar.events` and IMAP-via-app-password.
- **Consent screen attribution.** Your Google security page will report this as Mozilla
  Thunderbird. If you ever audit connected apps, you'll need to remember that's you.
