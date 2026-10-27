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
   Existing human auth-management workflows remain separate from mail/calendar
   confirmation; the trusted agent auth guard below requires confirmed removal.
   Enabling credential management is a privileged human decision.
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

## Trusted agent auth guard

`DOCKET_AGENT_AUTH=1` adds handler restrictions for an operator-authorized,
auth-only execution context. It does **not** change `CheckPermission`, create
permission grants, clear any legacy ceiling, or sandbox the binary by itself.
Exactly `1` enables it. A trusted runner must set it unconditionally for its
login/add/remove branch, reject every other operation there, and prevent caller
control of environment, executable, config, state paths and credential-owning UID.
The normal mail/calendar branch must retain its existing read-only/owned-event
ceilings. If those ceilings are set in the auth context, credential management
remains denied. Never clear them as a consequence of a caller-supplied flag.

- Login and add require the **original explicit** `--account <new-profile>`,
  before implicit account resolution. `default` is always reserved, even on an
  empty installation. Names are local profile labels, not Google identities;
  the human selects and consents to the identity. This does not enforce distinct
  Google accounts or identity-level isolation.
- Registry entries and any existing profile path (including empty directories,
  invalid tokens and dangling symlinks) block enrollment. Filesystem errors,
  symlink directories/registry and corrupt registry fail closed. These checks
  read only registry/name metadata, never credentials, before any OAuth listener.
- After consent the name is rechecked under the shared registry lock. An exclusive
  profile-directory creation and synced `0600` temporary-file hard-link publication
  refuse concurrent replacement. Registry updates use that same lock, preserving
  unrelated registrations. Local state must support hard links and file locks.
- If installation succeeds but registration/directory sync fails, the token is
  preserved and `ACCOUNT_INSTALL` (exit 1, non-retryable) explicitly requests human
  recovery. There is no automatic credential rollback or overwrite. A failed
  creation can leave an empty reserved directory after interruption; reconcile it
  manually. Direct writes by humans/root are outside guarded-flow serialization.
- Remove requires explicit `--account` and a true `--confirm` (otherwise
  `CONFIRM_REQUIRED`, exit 6, with no token/registry changes). Policy and all
  account/legacy ceilings still apply. Successful removal removes the token,
  registry entry, lock file and empty profile directory, not arbitrary contents;
  it neither revokes Google consent nor cancels in-flight requests. The name may
  then be reused, except reserved `default`. Unexpected directory contents can
  require human cleanup. Import/export always return `PERMISSION_DENIED` (exit 6)
  under the guard, regardless of operation grants.

Missing/reserved/occupied/unsafe login names return `ACCOUNT_SELECTION` (exit 2)
before OAuth. Without the guard, authorized human replacement/removal workflows
remain compatible; `--confirm` is accepted but optional for human removal.

Use the **same immutable config and canonical XDG_CONFIG_HOME/registry** for the
auth-only and normal contexts. Grant `auth.login` and `auth.remove` explicitly for
the intended profiles (or defaults covering new names), not import/export. Keep
`read_only_accounts` and any per-account denials: a new profile inherits its
name-based policy, so reserve privileged names via explicit denials if needed.
The operator must review that policy and the runner's exact allowlist together.
The Nix package advertises `agentAuthGuardVersion = 1`; runners should require
that capability before enabling this route, since older binaries ignore the guard.
A separate XDG config home would silently split registry discovery and locking.
This repository change does not configure or deploy such a runner.

## Safe rollout / Nix integration

This feature alone grants **no live writes**. No token re-login, format migration,
or new dependency versions are required. The package vendor hash is refreshed for
the local Gmail module source; force-rebuild `.#default.goModules` when validating
such source changes rather than trusting a cached fixed-output dependency. Review operator-generated
TOML with the new strict parser before rolling out (old misspelled/unused keys
are now errors). Roll out Docket and desired immutable policy together; leaving
legacy settings unchanged is safe but disables previously allowed writes/auth
management until explicitly granted. Do not loosen read-only ceilings to make a
denial disappear without a separate human policy decision.

### naboo (`hosts/naboo/services/beltino/docket-agent.nix`)

The current module copies `gmail/config/config.toml`, replaces the exact
`# read_only_accounts = ["archive"]` line with `read_only_accounts = ["work"]`,
and links the TOML into the **separate `docket-agent` user's** config directory.
That format and replacement remain compatible. Preserve the runner's sudo
boundary, `env_reset` and provider-override clearing. Operation grants alone must
not widen its allowlist. A separately authorized login/add/remove route must use
the trusted auth guard above; import/export stay excluded.

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
disabled. Never grant send/reply or general credential-management access for the
fleet. Owner-authorized new-profile login/confirmed removal is limited to the
separate guarded context described above.

The runner currently only creates its config symlink when absent: a future Nix
activation must atomically retarget an existing old symlink to the new store file,
rather than assume rebuilding the derivation updates it. Preserve owner/mode and
the writable `accounts.json` directory; do not make policy agent-writable. Pin the
reviewed Docket revision in Nix and build/evaluate the runner on both sides of a
policy change before any human-approved activation. No companion Nix PR is needed
for other read-only operations; attachment downloads now fail closed until the
fixed output root is wired as described below. No Nix/live config changes are made
by this Docket implementation.

### kirin / gala (`home/mac-common.nix`)

Home Manager installs Docket and exports mail-read-only/owned-events flags but does
not currently own settings. Retain those flags. An operator may add the above
three calendar grants to existing TOML, or declaratively generate/merge that file
with `pkgs.formats.toml` while preserving provider/calendar/account settings.
Do not replace existing provider settings or confuse `accounts.json` with policy.
Human credential-management commands need explicit grants and must be run outside
restricted agent environments; do not weaken the privileged naboo auth allowlist.

## Attachment output boundary

`mail.attachment` remains an independently authorized read operation, but its local
output must not become a way to replace runner policy/config symlinks, registry,
tokens (including newly created token paths), or shell dotfiles. Authorization and
config/account metadata discovery still run first. After flag validation and
before token reads/migration, supporting Gmail requests or downloads, the CLI
validates and opens the destination parent.

Set the operator-only environment variable `DOCKET_ATTACHMENT_ROOT` to an
**absolute, dedicated directory**. Relative `--out` paths resolve beneath it;
absolute paths must be within it. All creation, cleanup and atomic rename use a
pinned `os.Root` parent, not a checked string followed by an unrooted write.
Traversal/parent symlink escapes and replacing the root itself are refused. The
final entry may be replaced, including a symlink, but its target is never followed.
Existing in-root files can be overwritten; output remains mode `0600`. Directory
creation is an operator responsibility. Do not put config, account registry,
credentials, dotfiles or other privileged state anywhere in this tree, and do not
bind-mount such state into it (`os.Root` is not a mount/device sandbox).

If **any** of `DOCKET_READONLY`, `DOCKET_MAIL_READONLY`, `DOCKET_CAL_READONLY` or
`DOCKET_CAL_OWN_EVENTS_ONLY` is nonempty (including `0`) without an output root,
attachment output is refused with `OUTPUT_WRITE_FAILED` (exit 1, not retryable).
This keeps the current restricted runner safe before companion deployment wiring;
other allowed reads are unchanged. An unrestricted trusted same-UID CLI without
a root retains arbitrary output paths: its credentials are not a sandbox boundary.
The root is not a caller flag and must not be caller-overridable in a privileged
runner; preserve sudo `env_reset` and clear/reject inherited overrides before
setting the fixed value.

### Companion Nix wiring and delivery limits

The separately reviewed companion naboo Nix change should create
`/var/lib/docket-agent/attachments` owned by `docket-agent`, mode `0700`, and set
`DOCKET_ATTACHMENT_ROOT=/var/lib/docket-agent/attachments` inside the trusted
runner. Keep it separate from protected config/state and ensure its parents are
operator-controlled. Preserve existing permission/confirmation/ownership ceilings
and auth allowlists. This source change intentionally modifies no Nix or live
configuration; until that companion is deployed, attachments are unavailable in
restricted environments rather than writable to arbitrary runner-owned paths.

A successful JSON result reports a path owned by the protected writing UID. It
**does not** imply the untrusted caller can read it, nor does this change add a
file-delivery endpoint, broaden ACLs, copy data across UIDs, or stream private
attachments to stdout. If delivery is needed, separately review an authenticated,
account-authorized, narrowly scoped handoff and retention/cleanup policy. Do not
make the directory/world files readable or silently expose private mail merely
to make a reported path accessible. Same-UID restricted deployments likewise
need an operator-selected dedicated root if attachments are required.

## Verification

`nix develop --command make test vet build` tests both Go modules. Tests cover each
operation/account bit independently, unknown/malformed config, inheritance and
legacy ceilings, existing TOML preservation/JSON non-precedence, account placement
and ambiguity, auth aliases/list preflight, denied sensitive I/O/token migration,
receipt labelling versus sending, confirmation/dry-run and owned-event refusals.
HTTP-based CLI tests use fake credentials and local/fake transports, never real
mailboxes. Attachment regressions cover protected config symlinks, registry/token
and new-token destinations, traversal/absolute/parent-symlink escapes, final
symlink replacement, pinned-parent swaps, relative/absolute in-root downloads,
missing deployment roots and refused destinations before token reads/migration
or network access. `nix flake check` and `nix build` validate the package/vendor integration.
