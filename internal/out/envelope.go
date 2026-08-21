// Package out defines the JSON result envelope and exit codes returned to
// agent callers, and the TTY detection used to switch to human-readable output.
package out

import (
	"encoding/json"
	"fmt"
	"io"
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

// Page tells a caller whether it is holding the whole result set. It rides on
// the envelope rather than inside Data so each command keeps its own data
// shape (a bare array for search, an object for read): a caller that only
// wants results is untouched, and a caller that must not silently lose
// results has HasMore to check and NextPageToken to act on.
//
// Without it, a query capped at N is indistinguishable from one that happened
// to match exactly N, and the only defence available to a caller is to guess.
type Page struct {
	Returned      int    `json:"returned"`
	Limit         int64  `json:"limit"`
	HasMore       bool   `json:"has_more"`
	NextPageToken string `json:"next_page_token,omitempty"`
}

// Envelope is the top-level JSON shape every command emits on stdout.
type Envelope struct {
	OK       bool     `json:"ok"`
	Data     any      `json:"data"`
	Page     *Page    `json:"page,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Error    *Error   `json:"error"`
}

// Result is everything a successful command hands to the output layer.
// Verbose gates only the terminal rendering (see EmitResult).
type Result struct {
	Data     any
	Page     *Page
	Warnings []string
	Verbose  bool
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
	return EmitResult(Result{Data: data, Warnings: warnings})
}

// EmitVerbose is Emit with the human-readable table/key-value output forced
// to include verbose fields (struct fields tagged `verbose:"…"`). JSON
// output is unaffected — verbosity only gates the terminal rendering, so an
// agent piping to JSON always sees every field. Commands expose this as a
// --verbose flag.
func EmitVerbose(data any, warnings ...string) int {
	return EmitResult(Result{Data: data, Warnings: warnings, Verbose: true})
}

// EmitResult writes a successful result and returns the process exit code.
func EmitResult(r Result) int {
	if IsTTY() {
		for _, w := range r.Warnings {
			fmt.Fprintln(os.Stdout, "Warning:", w)
		}
		writeTable(os.Stdout, r.Data, r.Verbose)
		writePageNote(os.Stdout, r.Page)
		return ExitOK
	}
	writeJSON(os.Stdout, Envelope{OK: true, Data: r.Data, Page: r.Page, Warnings: r.Warnings})
	return ExitOK
}

// Fail writes a failure to stdout and returns the given exit code: JSON
// when stdout isn't a TTY, a short human-readable message when it is.
func Fail(code int, errCode, message string, retryable bool) int {
	code = failureExitCode(code)
	if IsTTY() {
		fmt.Fprintf(os.Stdout, "Error: %s\n  code: %s\n  retryable: %v\n", message, errCode, retryable)
		return code
	}
	writeJSON(os.Stdout, Envelope{OK: false, Error: &Error{Code: errCode, Message: message, Retryable: retryable}})
	return code
}

// unknownErrorCode classifies a failure that reached serialisation without
// describing itself. It should never appear in output; when it does, the
// caller at least gets something to branch on and a code to grep for.
const unknownErrorCode = "UNKNOWN_ERROR"

const unknownErrorMessage = "the command failed without reporting why; this is a docket bug — " +
	"re-run with the same arguments and, if it recurs, report the exact command"

// enforceContract applies the two invariants every caller parses against:
// ok == false carries a non-nil error with a populated code and message, and
// ok == true carries no error at all.
//
// It is applied here, at the one point where an envelope becomes bytes, so no
// future command can emit a failure a client cannot classify. A client that
// reads ok:false and a null error has nothing to act on — it cannot tell a
// rate limit it should back off from an empty result set it should accept —
// and repairing at each call site instead would leave the next call site free
// to get it wrong.
func enforceContract(env Envelope) Envelope {
	if env.OK {
		env.Error = nil
		return env
	}
	env.Data = nil
	if env.Error == nil {
		env.Error = &Error{}
	}
	if env.Error.Code == "" {
		env.Error.Code = unknownErrorCode
	}
	if env.Error.Message == "" {
		env.Error.Message = unknownErrorMessage
	}
	return env
}

// failureExitCode keeps a failure's exit status non-zero. A caller that
// checks only the exit status would read ExitOK as success and go on to
// parse a null data field as an empty result.
func failureExitCode(code int) int {
	if code == ExitOK {
		return ExitError
	}
	return code
}

func writeJSON(w io.Writer, env Envelope) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(enforceContract(env)); err != nil {
		fmt.Fprintln(os.Stderr, "docket: failed to encode output:", err)
	}
}

// writePageNote tells a human reader that they are looking at a partial
// result, mirroring what the JSON page object tells an agent.
func writePageNote(w io.Writer, p *Page) {
	if p == nil || !p.HasMore {
		return
	}
	if p.NextPageToken == "" {
		fmt.Fprintf(w, "\n(%d shown; more results exist)\n", p.Returned)
		return
	}
	fmt.Fprintf(w, "\n(%d shown; more results exist — re-run with --page-token %s)\n",
		p.Returned, p.NextPageToken)
}
