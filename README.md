# Docket

An agent-facing CLI for Gmail and Google Calendar with structured output.

Build from this checkout with `nix build`. The binary is `./result/bin/docket`.

```text
docket [--account <name>] <auth|mail|cal> <subcommand> [flags]
```

## Examples

After [account setup](permissions.md#first-time-account-setup), run `docket` from your `PATH`. Output is human-readable in a terminal and JSON when piped or redirected.

```console
$ docket --account default mail search --query 'from:alex@example.com is:unread' --limit 5
ID      THREAD_ID  FROM              TO                  SUBJECT       DATE  LABELS         SNIPPET
msg123  thread123  alex@example.com  reader@example.com  Launch notes        INBOX, UNREAD  The draft is ready for review.

$ docket --account default mail read --id msg123
…
body:            The draft is ready for review.
…

$ docket --account default cal list
ID                  NAME
reader@example.com  Personal
```

[Mail](docket-design.md#4-mail) · [Calendar](docket-design.md#5-calendar) · [Output format](docket-design.md#6-agent-interface-contract) · [Permissions](permissions.md)
