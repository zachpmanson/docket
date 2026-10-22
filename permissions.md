# Per-account operation permissions

Docket's **CLI** authorizes each command before entering its handler. This covers
reads as well as writes, credential operations, previews, stdin/body files and
attachment output. The reusable Gmail library is not a security sandbox: callers
with direct access to tokens can bypass any CLI policy. Keep credentials and the
policy outside an untrusted caller's UID, as naboo's privileged runner does.

## Settings schema (TOML, not JSON)

Settings remain `$XDG_CONFIG_HOME/docket/config.toml` (default
`~/.config/docket/config.toml`). TOML already supports nested maps and Nix's
`pkgs.formats.toml` can generate them, so a JSON migration adds no useful capability
and would introduce an unnecessary second policy source.

**No settings files are migrated or overwritten.** Only `config.toml` is read;
`config.json` is ignored, even if both files exist. `accounts.json` is still the
account-name/verified-email registry, **not policy**. State `token.json` files are
credentials, not settings. Provider fields, `default_calendar` and
`read_only_accounts` retain their names and semantics.

```toml
# Top-level keys must precede any table header.
read_only_accounts = ["work"]
default_calendar = "primary"

[provider]
# Retain your existing provider fields here (see gmail/config/config.toml).
client_id = "your-client-id"

[permissions.defaults]
# Optional per-operation defaults for every account, including future profiles.
"mail.attachment" = false

[permissions.accounts.default]
# Example ONLY: allow personal receipt labelling, not sending/replying.
"mail.label" = true
"mail.send" = false
"mail.reply" = false
"mail.attachment" = true

[permissions.accounts.work]
# work inherits read permissions, and its legacy read-only ceiling still wins
# over any write grants, whether here or in permissions.defaults.
"mail.attachment" = false
```

Each quoted key is one exact operation name, with a TOML **boolean** value. Quote
names containing dots: `"mail.read" = false`, not `mail.read = false`. There are no
wildcards, group grants, string booleans, or implicit account creation from policy.
Account names follow `--account` validation: 1–64 letters/numbers/`_`/`-`, first
character alphanumeric; case-sensitive.

### Complete operation matrix

| Surface | Exact permission names | Built-in default |
|---|---|---|
| Mail reads | `mail.search`, `mail.list`, `mail.read`, `mail.thread`, `mail.attachment` | allow |
| Mail writes | `mail.send`, `mail.reply`, `mail.label` | **deny** |
| Calendar reads | `cal.list`, `cal.search`, `cal.agenda`, `cal.show`, `cal.freebusy`, `cal.find-slot` | allow |
| Calendar writes | `cal.create`, `cal.update`, `cal.delete` | **deny** |
| Account inspection | `auth.whoami`, `auth.accounts` | allow |
| Credential management | `auth.login`, `auth.import`, `auth.export`, `auth.remove` | **deny** |

`auth add` is an alias of `auth.login`; `auth list` is an alias of `auth.accounts`.
Aliases cannot bypass policy. Every other operation is independently toggleable.
Operation permissions cover the full command, including supporting reads (e.g.
`mail.reply` reads the original and mailbox aliases; `cal.freebusy` reads events;
`auth.accounts` probes identity and records verified emails). Denying `mail.read`
does not redact message content returned by a separately allowed `mail.thread`.
Granting one permission never grants a sibling permission.

### Defaults, precedence and validation

1. Built-in defaults above apply even if the entire `permissions` section is absent.
   **Upgrade tightening:** legacy mail/calendar writes and credential management
   now need explicit grants. Existing reads remain available unless denied.
2. `permissions.defaults` overrides built-in defaults operation by operation.
3. `permissions.accounts.<name>` overrides those defaults for that exact account.
   An omitted account/key inherits; `false` is an explicit denial.
4. Legacy upper bounds always win over grants:
   - Any nonempty `DOCKET_READONLY`: all mail/calendar writes disabled.
   - Any nonempty `DOCKET_MAIL_READONLY` or `DOCKET_CAL_READONLY`: that surface's
     writes disabled. Even `"0"` counts as set.
   - `read_only_accounts`: all mail/calendar writes disabled for listed accounts.
   - **Credential management** is blocked by **any** read-only environment flag,
     by `read_only_accounts` for that account, or by `DOCKET_CAL_OWN_EVENTS_ONLY`.
     Otherwise token export/replacement could escape these restrictions.
5. Authorized mail/calendar mutations still require `--confirm`; `--dry-run`
   previews only, even with `--confirm`. Neither flag overrides a denial.
   Existing auth-management workflows remain separate from mail/calendar
   confirmation: enabling them is a privileged human decision.
6. `DOCKET_CAL_OWN_EVENTS_ONLY` still enforces the `[docket]` marker for authorized
   calendar updates/deletes, including their previews. Grants cannot override it.

Unknown config fields/operations/accounts with invalid names, bad types, duplicate
keys, and malformed TOML fail with `CONFIG_ERROR` (exit 2), before credentials or
network access. Unknown future operations fail closed in the evaluator; new CLI
commands must be added to the matrix and the dispatcher together. A config file
is never partially applied, and a bad file never falls back to embedded defaults.

`PERMISSION_DENIED` (exit 6, `retryable: false`) names the selected account and exact
operation and directs the caller to human policy review. Legacy ceilings retain
`WRITES_DISABLED` with the same account/operation detail. No protected reads,
network request, token export/migration, body-file/stdin access or preview occurs
on denial. Config parsing and account-name discovery (registry metadata and token
file **stat**, not token contents) necessarily precede authorization.

## Account selection

`--account name` / `--account=name` can precede the domain, occur between domain
and subcommand, or follow the subcommand. Actual flag values are not interpreted
as account selectors (e.g. `--query '--account=work'` is query text). Repeated,
missing and invalid selectors fail. Zero profiles uses `default`; one selects it;
multiple require explicit selection. Selection never depends on connectivity and
never falls back after auth failure. The authorized account/config snapshot is
threaded through the command, including its confirmation hint.

`auth login`/`add`/`import` can select a new profile, but check that new name's
policy **before** stdin, OAuth or token writes. `add` and `remove` require an
explicit account. `auth accounts`/`list` without a selector is deliberately a
multi-account operation: preflight **all** configured accounts before probing any.
If any is denied the whole listing is refused, not partially returned. Use
`auth list --account default` to inspect only a permitted profile. With no profiles,
listing checks the `default` policy and returns an empty list.

## Safe rollout / Nix integration

This feature alone grants **no live writes**. No token re-login, format migration,
new dependencies, or vendor-hash change is required. Review operator-generated
TOML with the new strict parser before rolling out (old misspelled/unused keys
are now errors). Roll out Docket and desired immutable policy together; leaving
legacy settings unchanged is safe but disables previously allowed writes/auth
management until explicitly granted. Do not loosen read-only ceilings to make a
denial disappear without a separate human policy decision.

### naboo (`hosts/naboo/services/beltino/docket-agent.nix`)

The current module copies `gmail/config/config.toml`, replaces the exact
`# read_only_accounts = ["archive"]` line with `read_only_accounts = ["work"]`,
and links the TOML into the **separate `docket-agent` user's** config directory.
That format and replacement remain compatible. The runner's sudo boundary,
`env_reset`, provider-override clearing and `auth whoami`-only allowlist must stay.
Do not permit auth export/import/login/removal just because operation grants exist.

To retain the prior personal owned-calendar-write functionality after the version
bump, the **companion Nix source change** (reviewed separately; not performed here)
is to append this to `configuredConfig` in that module, or generate the equivalent
TOML with `pkgs.formats.toml`:

```toml
[permissions.accounts.default]
"cal.create" = true
"cal.update" = true
"cal.delete" = true
```

Keep `read_only_accounts = ["work"]`, `DOCKET_MAIL_READONLY=1` and
`DOCKET_CAL_OWN_EVENTS_ONLY=1`. This restores the existing calendar ceiling, not
mail writes. A subsequent, **separately authorized** receipt-labelling policy would
also grant only `"mail.label" = true` for `default` **and** change the runner's
mail-wide kill switch; until both are explicitly reviewed, mail writes remain
disabled. Never grant send/reply or credential-management permissions for the fleet.

The runner currently only creates its config symlink when absent: a future Nix
activation must atomically retarget an existing old symlink to the new store file,
rather than assume rebuilding the derivation updates it. Preserve owner/mode and
the writable `accounts.json` directory; do not make policy agent-writable. Pin the
reviewed Docket revision in Nix and build/evaluate the runner on both sides of a
policy change before any human-approved activation. No companion Nix PR is needed
merely to retain read-only compatibility; no Nix/live config changes are made by
this Docket implementation.

### kirin / gala (`home/mac-common.nix`)

Home Manager installs Docket and exports mail-read-only/owned-events flags but does
not currently own settings. Retain those flags. An operator may add the above
three calendar grants to existing TOML, or declaratively generate/merge that file
with `pkgs.formats.toml` while preserving provider/calendar/account settings.
Do not replace existing provider settings or confuse `accounts.json` with policy.
Human credential-management commands need explicit grants and must be run outside
restricted agent environments; do not weaken the privileged naboo auth allowlist.

## Verification

`nix develop --command make test vet build` tests both Go modules. Tests cover each
operation/account bit independently, unknown/malformed config, inheritance and
legacy ceilings, existing TOML preservation/JSON non-precedence, account placement
and ambiguity, auth aliases/list preflight, denied sensitive I/O/token migration,
receipt labelling versus sending, confirmation/dry-run and owned-event refusals.
HTTP-based CLI tests use fake credentials and local/fake transports, never real
mailboxes. `nix flake check` and `nix build` validate the package/vendor integration.
