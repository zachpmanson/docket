// Package errors owns the process exit codes shared by every docket
// command. They live in the library — not the CLI shell — so a consumer
// of the gmail package can classify failures with the same semantics
// docket uses. See docket-design.md §6.
package errors

// Exit codes shared across all commands. See docket-design.md §6.
const (
	ExitOK             = 0
	ExitError          = 1
	ExitUsage          = 2
	ExitAuthRequired   = 3
	ExitNotFound       = 4
	ExitRateLimited    = 5
	ExitConfirmMissing = 6
)
