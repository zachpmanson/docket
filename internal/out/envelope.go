// Package out defines the JSON result envelope and exit codes returned to
// agent callers, and the TTY detection used to switch to human-readable output.
package out

import (
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/term"
)

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

// Error is the machine-readable error shape carried in a failed Envelope.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Envelope is the top-level JSON shape every command emits on stdout.
type Envelope struct {
	OK       bool     `json:"ok"`
	Data     any      `json:"data"`
	Warnings []string `json:"warnings,omitempty"`
	Error    *Error   `json:"error"`
}

// IsTTY reports whether stdout is an interactive terminal. Commands use this
// to decide between JSON and a human-readable table.
func IsTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// Emit writes a successful result to stdout and returns the process exit
// code: JSON when stdout isn't a TTY, a human-readable table when it is.
// See docket-design.md §1 principle 1.
func Emit(data any, warnings ...string) int {
	return EmitOptions(data, false, warnings...)
}

// EmitVerbose is Emit with the human-readable table/key-value output forced
// to include verbose fields (struct fields tagged `verbose:"…"`). JSON
// output is unaffected — verbosity only gates the terminal rendering, so an
// agent piping to JSON always sees every field. Commands expose this as an
// --all flag.
func EmitVerbose(data any, warnings ...string) int {
	return EmitOptions(data, true, warnings...)
}

// EmitOptions is the shared core of Emit/EmitVerbose.
func EmitOptions(data any, verbose bool, warnings ...string) int {
	if IsTTY() {
		for _, w := range warnings {
			fmt.Fprintln(os.Stdout, "Warning:", w)
		}
		writeTable(os.Stdout, data, verbose)
		return ExitOK
	}
	env := Envelope{OK: true, Data: data, Warnings: warnings}
	writeJSON(env)
	return ExitOK
}

// Fail writes a failure to stdout and returns the given exit code: JSON
// when stdout isn't a TTY, a short human-readable message when it is.
func Fail(code int, errCode, message string, retryable bool) int {
	if IsTTY() {
		fmt.Fprintf(os.Stdout, "Error: %s\n  code: %s\n  retryable: %v\n", message, errCode, retryable)
		return code
	}
	env := Envelope{OK: false, Data: nil, Error: &Error{Code: errCode, Message: message, Retryable: retryable}}
	writeJSON(env)
	return code
}

func writeJSON(env Envelope) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		fmt.Fprintln(os.Stderr, "docket: failed to encode output:", err)
	}
}
