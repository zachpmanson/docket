# Docket

An agent-facing CLI for Gmail and Google Calendar with structured output.

Build from this checkout with `nix build`. The binary is `./result/bin/docket`.

```text
docket [--account <name>] <auth|mail|cal> <subcommand> [flags]
```

[Account setup and permissions](permissions.md#first-time-account-setup) · [Mail](docket-design.md#4-mail) · [Calendar](docket-design.md#5-calendar) · [Output format](docket-design.md#6-agent-interface-contract)
