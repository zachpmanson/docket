# Docket

An agent-facing CLI for Gmail and Google Calendar with structured output.

Build from this checkout with `nix build`. The binary is `./result/bin/docket`.

```text
docket [--account <name>] <auth|mail|cal> <subcommand> [flags]
```

## Examples

After [account setup](permissions.md#first-time-account-setup), search Gmail, read a result by ID, or discover calendar IDs. These examples use fictional data; `jq` shows selected fields from Docket's JSON output.

```console
$ ./result/bin/docket --account default mail search \
    --query 'from:alex@example.com is:unread' --limit 5 | jq -c '.data[] | {id, subject}'
{"id":"msg123","subject":"Launch notes"}

$ ./result/bin/docket --account default mail read --id msg123 | jq -r '.data.body'
The draft is ready for review.

$ ./result/bin/docket --account default cal list | jq -c '.data[]'
{"id":"reader@example.com","name":"Personal"}
```

[Mail](docket-design.md#4-mail) · [Calendar](docket-design.md#5-calendar) · [Output format](docket-design.md#6-agent-interface-contract) · [Permissions](permissions.md)
