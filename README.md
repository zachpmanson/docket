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
| `mail read --id <gm-msgid>` | Full body; prefers `text/plain`, truncates at `--max-bytes` |
| `mail thread --id <gm-thrid>` | Read a whole conversation |
| `mail send --to ... --subject ... --body-file -` | Send (mutating: `--confirm`) |
| `mail reply --id <gm-msgid> --body-file -` | Reply (mutating: `--confirm`) |
| `mail label --id <gm-msgid> --add Foo --remove INBOX` | Apply/remove labels (mutating: `--confirm`) |

`search`/`list` return envelopes only (id, thread id, from/to, subject, date, labels,
snippet) — bodies are expensive, so callers ask for them explicitly with `read`.

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

```json
{ "ok": false, "data": null,
  "error": { "code": "AUTH_EXPIRED", "message": "refresh token rejected", "retryable": false } }
```

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
