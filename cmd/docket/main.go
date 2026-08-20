// Command docket is a single binary exposing Gmail and Google Calendar as
// composable subcommands with structured output, designed to be driven by
// an LLM agent. See docket-design.md for the full design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-webdav/caldav"
	"github.com/teambition/rrule-go"
	"google.golang.org/api/gmail/v1"

	"github.com/zachpmanson/docket/internal/auth"
	"github.com/zachpmanson/docket/internal/cal"
	"github.com/zachpmanson/docket/internal/mail"
	"github.com/zachpmanson/docket/internal/out"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		return usageError("missing command", "docket <auth|mail|cal> <subcommand> [flags]")
	}

	ctx := context.Background()

	switch args[0] {
	case "auth":
		return runAuth(ctx, args[1:])
	case "mail":
		return runMail(ctx, args[1:])
	case "cal":
		return runCal(ctx, args[1:])
	default:
		return usageError(
			fmt.Sprintf("unknown command %q", args[0]),
			"docket <auth|mail|cal> <subcommand> [flags]; valid commands: auth, mail, cal")
	}
}

// usageError produces an LLM-friendly usage failure: what was wrong with
// this invocation, and what a corrected call looks like. See
// docket-design.md §1 principle 7.
func usageError(problem, correctUsage string) int {
	msg := fmt.Sprintf("%s. Usage: %s", problem, correctUsage)
	return out.Fail(out.ExitUsage, "USAGE_ERROR", msg, false)
}

// newFlagSet builds a FlagSet for a subcommand with its default usage
// output suppressed. Without this, the standard library prints its own
// "flag provided but not defined" + flag-by-flag usage text straight to
// stderr on every parse error — noise that duplicates (inconsistently
// formatted) what usageError already puts in the JSON envelope. See
// docket-design.md §1 principle 7: errors are for the agent, not a
// terminal, and every response should be the one structured envelope.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// writeGate enforces the write-safety contract every mutating command
// shares, in order: an operator kill switch (env var, for a deployment
// where a human wants to disable writes without touching agent-facing
// flags), then --dry-run (preview only, nothing executes), then --confirm
// (required to actually execute). See docket-design.md §1 principles 3 and
// 7, and §6 exit code 6.
//
// DOCKET_READONLY disables all writes; DOCKET_MAIL_READONLY/
// DOCKET_CAL_READONLY scope it to one surface. Any non-empty value counts
// as set.
func writeGate(kind, disableEnvVar string, confirm, dryRun bool, preview any, rerun string) (proceed bool, code int) {
	if os.Getenv("DOCKET_READONLY") != "" || os.Getenv(disableEnvVar) != "" {
		return false, out.Fail(out.ExitConfirmMissing, "WRITES_DISABLED",
			fmt.Sprintf("%s writes are administratively disabled on this deployment "+
				"(DOCKET_READONLY or %s is set) — a human must unset it to allow this", kind, disableEnvVar),
			false)
	}
	if dryRun {
		return false, out.Emit(map[string]any{"dry_run": true, "preview": preview})
	}
	if !confirm {
		return false, out.Fail(out.ExitConfirmMissing, "CONFIRM_REQUIRED",
			"this is a mutating command and requires --confirm to execute; re-run exactly as "+
				"shown but with --confirm added: "+rerun, false)
	}
	return true, out.ExitOK
}

// calOwnEventsOnly reports whether this deployment restricts cal update/
// delete to events docket itself created (see docket-design.md §5's "soft
// write" mode) — a middle ground between fully read-only and full write
// access, e.g. for an agent that should manage its own calendar entries but
// never touch anything a human created directly.
func calOwnEventsOnly() bool {
	return os.Getenv("DOCKET_CAL_OWN_EVENTS_ONLY") != ""
}

// withOptionalFlag appends --name "value" to cmd if value is non-empty, for
// building a --confirm rerun hint that reflects every flag the agent
// actually passed — dropping one here means the "re-run exactly as shown"
// promise in writeGate silently produces a different result than what was
// just previewed.
func withOptionalFlag(cmd, name, value string) string {
	if value == "" {
		return cmd
	}
	return fmt.Sprintf("%s --%s %q", cmd, name, value)
}

// splitCommaList splits a comma-separated flag value, trimming whitespace
// and dropping empty entries, so "" and ",," both yield an empty slice
// rather than a slice of empty strings.
func splitCommaList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// readBodyFile reads path, or stdin if path is "-".
func readBodyFile(path string) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading body from stdin: %w", err)
		}
		return string(b), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading --body-file %q: %w", path, err)
	}
	return string(b), nil
}

func runAuth(ctx context.Context, args []string) int {
	if len(args) < 1 {
		return usageError("missing auth subcommand", "docket auth <login|whoami|import|export>")
	}
	switch args[0] {
	case "login":
		return cmdLogin(ctx)
	case "whoami":
		return cmdWhoAmI(ctx)
	case "import":
		return cmdImport()
	case "export":
		return cmdExport()
	default:
		return usageError(
			fmt.Sprintf("unknown auth subcommand %q", args[0]),
			"docket auth <login|whoami|import|export>")
	}
}

func runMail(ctx context.Context, args []string) int {
	if len(args) < 1 {
		return usageError("missing mail subcommand",
			"docket mail <search|list|read|thread|send|reply|label> [flags]")
	}
	switch args[0] {
	case "search":
		return cmdMailSearch(ctx, args[1:])
	case "list":
		return cmdMailList(ctx, args[1:])
	case "read":
		return cmdMailRead(ctx, args[1:])
	case "thread":
		return cmdMailThread(ctx, args[1:])
	case "send":
		return cmdMailSend(ctx, args[1:])
	case "reply":
		return cmdMailReply(ctx, args[1:])
	case "label":
		return cmdMailLabel(ctx, args[1:])
	default:
		return usageError(
			fmt.Sprintf("unknown mail subcommand %q", args[0]),
			"docket mail <search|list|read|thread|send|reply|label> [flags]")
	}
}

func runCal(ctx context.Context, args []string) int {
	if len(args) < 1 {
		return usageError("missing cal subcommand",
			"docket cal <agenda|show|freebusy|find-slot|create|update|delete> [flags]")
	}
	switch args[0] {
	case "agenda":
		return cmdCalAgenda(ctx, args[1:])
	case "show":
		return cmdCalShow(ctx, args[1:])
	case "freebusy":
		return cmdCalFreeBusy(ctx, args[1:])
	case "find-slot":
		return cmdCalFindSlot(ctx, args[1:])
	case "create":
		return cmdCalCreate(ctx, args[1:])
	case "update":
		return cmdCalUpdate(ctx, args[1:])
	case "delete":
		return cmdCalDelete(ctx, args[1:])
	default:
		return usageError(
			fmt.Sprintf("unknown cal subcommand %q", args[0]),
			"docket cal <agenda|show|freebusy|find-slot|create|update|delete> [flags]")
	}
}

// --- auth ---

func cmdLogin(ctx context.Context) int {
	cfg, err := auth.LoadConfig()
	if err != nil {
		return out.Fail(out.ExitError, "CONFIG_ERROR", err.Error(), false)
	}

	tok, err := auth.Login(ctx, cfg.Provider)
	if err != nil {
		return out.Fail(out.ExitAuthRequired, "AUTH_FAILED", err.Error(), false)
	}

	path, err := auth.TokenPath()
	if err != nil {
		return out.Fail(out.ExitError, "TOKEN_PATH", err.Error(), false)
	}
	if err := auth.SaveToken(tok, path); err != nil {
		return out.Fail(out.ExitError, "TOKEN_WRITE", err.Error(), false)
	}

	return out.Emit(map[string]any{"status": "logged in"})
}

func cmdWhoAmI(ctx context.Context) int {
	cfg, err := auth.LoadConfig()
	if err != nil {
		return out.Fail(out.ExitError, "CONFIG_ERROR", err.Error(), false)
	}
	src, err := auth.TokenSource(ctx, cfg)
	if err != nil {
		return out.Fail(out.ExitAuthRequired, "AUTH_REQUIRED", err.Error()+
			" (run `docket auth login` first)", false)
	}
	who, err := auth.WhoAmIFromToken(ctx, src)
	if err != nil {
		return out.Fail(out.ExitAuthRequired, "AUTH_EXPIRED", err.Error(), true)
	}
	return out.Emit(who)
}

func cmdImport() int {
	if err := auth.ImportToken(os.Stdin); err != nil {
		return out.Fail(out.ExitError, "IMPORT_FAILED", err.Error(), false)
	}
	return out.Emit(map[string]any{"status": "imported"})
}

func cmdExport() int {
	if err := auth.ExportToken(os.Stdout); err != nil {
		return out.Fail(out.ExitError, "EXPORT_FAILED", err.Error(), false)
	}
	return out.ExitOK
}

// --- mail ---

// mailContext builds the Gmail client and label cache shared by every mail
// subcommand. Returns a Fail exit code (never 0) on error.
func mailContext(ctx context.Context) (*gmail.Service, *mail.LabelCache, int) {
	cfg, err := auth.LoadConfig()
	if err != nil {
		return nil, nil, out.Fail(out.ExitError, "CONFIG_ERROR", err.Error(), false)
	}
	src, err := auth.TokenSource(ctx, cfg)
	if err != nil {
		return nil, nil, out.Fail(out.ExitAuthRequired, "AUTH_REQUIRED",
			err.Error()+" (run `docket auth login` first)", false)
	}
	svc, err := mail.NewService(ctx, src)
	if err != nil {
		return nil, nil, out.Fail(out.ExitError, "GMAIL_CLIENT_ERROR", err.Error(), false)
	}
	labels, err := mail.LoadLabels(ctx, svc)
	if err != nil {
		return nil, nil, out.Fail(out.ExitRateLimited, "GMAIL_API_ERROR", err.Error(), true)
	}
	return svc, labels, out.ExitOK
}

func cmdMailSearch(ctx context.Context, args []string) int {
	fs := newFlagSet("mail search")
	query := fs.String("query", "", "Gmail search query, e.g. \"from:emiel is:unread\"")
	limit := fs.Int64("limit", 25, "max results to return")
	all := fs.Bool("all", false, "include threading/cc headers in terminal output")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket mail search --query \"...\" [--limit 25] [--all]")
	}
	if *query == "" {
		return usageError("--query is required and must not be empty",
			"docket mail search --query \"from:emiel is:unread\" [--limit 25] [--all]")
	}

	svc, labels, code := mailContext(ctx)
	if code != out.ExitOK {
		return code
	}

	envelopes, err := mail.List(ctx, svc, labels, mail.ListOptions{Query: *query, Limit: *limit})
	if err != nil {
		return out.Fail(out.ExitError, "GMAIL_API_ERROR", err.Error(), true)
	}
	if *all {
		return out.EmitVerbose(envelopes)
	}
	return out.Emit(envelopes)
}

func cmdMailList(ctx context.Context, args []string) int {
	fs := newFlagSet("mail list")
	label := fs.String("label", "INBOX", "label name to list, e.g. INBOX")
	limit := fs.Int64("limit", 25, "max results to return")
	unread := fs.Bool("unread", false, "only unread messages")
	all := fs.Bool("all", false, "include threading/cc headers in terminal output")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket mail list --label INBOX [--limit 25] [--unread] [--all]")
	}

	svc, labels, code := mailContext(ctx)
	if code != out.ExitOK {
		return code
	}

	labelID, ok := labels.ID(*label)
	if !ok {
		return usageError(
			fmt.Sprintf("no label named %q", *label),
			fmt.Sprintf("docket mail list --label <name>; known labels: %v", labels.AllNames()))
	}

	ids := []string{labelID}
	if *unread {
		if unreadID, ok := labels.ID("UNREAD"); ok {
			ids = append(ids, unreadID)
		}
	}

	envelopes, err := mail.List(ctx, svc, labels, mail.ListOptions{LabelIDs: ids, Limit: *limit})
	if err != nil {
		return out.Fail(out.ExitError, "GMAIL_API_ERROR", err.Error(), true)
	}
	if *all {
		return out.EmitVerbose(envelopes)
	}
	return out.Emit(envelopes)
}

func cmdMailRead(ctx context.Context, args []string) int {
	fs := newFlagSet("mail read")
	id := fs.String("id", "", "Gmail message id, from a search/list/thread result")
	maxBytes := fs.Int("max-bytes", 20000, "truncate body at this many bytes")
	all := fs.Bool("all", false, "include threading/cc headers in terminal output")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket mail read --id <gm-id> [--max-bytes 20000] [--all]")
	}
	if *id == "" {
		return usageError("--id is required and must not be empty",
			"docket mail read --id <gm-id> [--max-bytes 20000] [--all]; ids come from "+
				"`mail search`/`mail list` output, not from a subject line or index")
	}

	svc, labels, code := mailContext(ctx)
	if code != out.ExitOK {
		return code
	}

	msg, err := mail.Read(ctx, svc, labels, *id, *maxBytes)
	if err != nil {
		return out.Fail(out.ExitNotFound, "MESSAGE_NOT_FOUND", err.Error(), false)
	}
	if *all {
		return out.EmitVerbose(msg)
	}
	return out.Emit(msg)
}

func cmdMailThread(ctx context.Context, args []string) int {
	fs := newFlagSet("mail thread")
	id := fs.String("id", "", "Gmail thread id, from a search/list/read result's thread_id")
	all := fs.Bool("all", false, "include threading/cc headers in terminal output")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket mail thread --id <gm-thread-id> [--all]")
	}
	if *id == "" {
		return usageError("--id is required and must not be empty",
			"docket mail thread --id <gm-thread-id> [--all]; thread ids come from an "+
				"Envelope's thread_id field, not the message id")
	}

	svc, labels, code := mailContext(ctx)
	if code != out.ExitOK {
		return code
	}

	thread, err := mail.GetThread(ctx, svc, labels, *id)
	if err != nil {
		return out.Fail(out.ExitNotFound, "THREAD_NOT_FOUND", err.Error(), false)
	}
	if *all {
		return out.EmitVerbose(thread)
	}
	return out.Emit(thread)
}

func cmdMailSend(ctx context.Context, args []string) int {
	fs := newFlagSet("mail send")
	to := fs.String("to", "", "recipient address(es), comma-separated")
	subject := fs.String("subject", "", "subject line")
	bodyFile := fs.String("body-file", "", "path to plain-text body, or - for stdin")
	confirm := fs.Bool("confirm", false, "actually send (required)")
	dryRun := fs.Bool("dry-run", false, "preview without sending")
	all := fs.Bool("all", false, "include threading/cc headers in terminal output")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket mail send --to ... --subject ... --body-file - [--confirm] [--all]")
	}
	if *to == "" || *subject == "" || *bodyFile == "" {
		return usageError("--to, --subject, and --body-file are all required",
			"docket mail send --to ... --subject ... --body-file - [--confirm] [--all]")
	}
	body, err := readBodyFile(*bodyFile)
	if err != nil {
		return usageError(err.Error(), "docket mail send --to ... --subject ... --body-file - [--confirm]")
	}

	svc, labels, code := mailContext(ctx)
	if code != out.ExitOK {
		return code
	}

	plan, err := mail.PrepareSend(*to, *subject, body)
	if err != nil {
		return usageError(err.Error(), "docket mail send --to ... --subject ... --body-file - [--confirm]")
	}

	rerun := fmt.Sprintf("docket mail send --to %q --subject %q --body-file %s --confirm", *to, *subject, *bodyFile)
	proceed, code := writeGate("mail", "DOCKET_MAIL_READONLY", *confirm, *dryRun, plan, rerun)
	if !proceed {
		return code
	}

	env, err := plan.Execute(ctx, svc, labels)
	if err != nil {
		return out.Fail(out.ExitError, "SEND_FAILED", err.Error(), true)
	}
	if *all {
		return out.EmitVerbose(env)
	}
	return out.Emit(env)
}

func cmdMailReply(ctx context.Context, args []string) int {
	fs := newFlagSet("mail reply")
	id := fs.String("id", "", "id of the message being replied to")
	bodyFile := fs.String("body-file", "", "path to plain-text body, or - for stdin")
	confirm := fs.Bool("confirm", false, "actually send (required)")
	dryRun := fs.Bool("dry-run", false, "preview without sending")
	all := fs.Bool("all", false, "include threading/cc headers in terminal output")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket mail reply --id <gm-id> --body-file - [--confirm] [--all]")
	}
	if *id == "" || *bodyFile == "" {
		return usageError("--id and --body-file are both required",
			"docket mail reply --id <gm-id> --body-file - [--confirm] [--all]")
	}
	body, err := readBodyFile(*bodyFile)
	if err != nil {
		return usageError(err.Error(), "docket mail reply --id <gm-id> --body-file - [--confirm]")
	}

	svc, labels, code := mailContext(ctx)
	if code != out.ExitOK {
		return code
	}

	plan, err := mail.PrepareReply(ctx, svc, *id, body)
	if err != nil {
		return out.Fail(out.ExitNotFound, "MESSAGE_NOT_FOUND", err.Error(), false)
	}

	rerun := fmt.Sprintf("docket mail reply --id %s --body-file %s --confirm", *id, *bodyFile)
	proceed, code := writeGate("mail", "DOCKET_MAIL_READONLY", *confirm, *dryRun, plan, rerun)
	if !proceed {
		return code
	}

	env, err := plan.Execute(ctx, svc, labels)
	if err != nil {
		return out.Fail(out.ExitError, "SEND_FAILED", err.Error(), true)
	}
	if *all {
		return out.EmitVerbose(env)
	}
	return out.Emit(env)
}

func cmdMailLabel(ctx context.Context, args []string) int {
	fs := newFlagSet("mail label")
	id := fs.String("id", "", "message id")
	add := fs.String("add", "", "comma-separated label names to add")
	remove := fs.String("remove", "", "comma-separated label names to remove")
	confirm := fs.Bool("confirm", false, "actually apply the change (required)")
	dryRun := fs.Bool("dry-run", false, "preview without applying")
	all := fs.Bool("all", false, "include threading/cc headers in terminal output")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket mail label --id <gm-id> --add Foo --remove INBOX [--confirm] [--all]")
	}
	if *id == "" {
		return usageError("--id is required and must not be empty",
			"docket mail label --id <gm-id> --add Foo --remove INBOX [--confirm] [--all]")
	}
	addNames := splitCommaList(*add)
	removeNames := splitCommaList(*remove)
	if len(addNames) == 0 && len(removeNames) == 0 {
		return usageError("at least one of --add or --remove is required",
			"docket mail label --id <gm-id> --add Foo --remove INBOX [--confirm]")
	}

	svc, labels, code := mailContext(ctx)
	if code != out.ExitOK {
		return code
	}

	plan, err := mail.PrepareLabel(labels, *id, addNames, removeNames)
	if err != nil {
		return usageError(err.Error(),
			"docket mail label --id <gm-id> --add <name> --remove <name> [--confirm]")
	}

	rerun := fmt.Sprintf("docket mail label --id %s --add %q --remove %q --confirm", *id, *add, *remove)
	proceed, code := writeGate("mail", "DOCKET_MAIL_READONLY", *confirm, *dryRun, plan, rerun)
	if !proceed {
		return code
	}

	env, err := plan.Execute(ctx, svc, labels)
	if err != nil {
		return out.Fail(out.ExitError, "LABEL_FAILED", err.Error(), true)
	}
	if *all {
		return out.EmitVerbose(env)
	}
	return out.Emit(env)
}

// --- cal ---

// calDefaultCalendar returns the configured default calendar id, or
// cal.PrimaryCalendar (the account's own calendar) when unset/unreadable.
// feeds the --calendar flag default in every cal subcommand, so the
// config's default_calendar key is what a bare `docket cal create ...`
// targets. An explicit --calendar flag still wins.
func calDefaultCalendar() string {
	cfg, err := auth.LoadConfig()
	if err != nil {
		return cal.PrimaryCalendar
	}
	if cfg.DefaultCalendar != "" {
		return cfg.DefaultCalendar
	}
	return cal.PrimaryCalendar
}

// calContext builds the CalDAV client shared by every cal subcommand, and
// resolves "primary" to the account's actual email address — Google's
// CalDAV interface has no literal "primary" calendar id the way the REST
// API does. Returns a Fail exit code (never 0) on error.
func calContext(ctx context.Context, calendarID string) (*caldav.Client, string, int) {
	cfg, err := auth.LoadConfig()
	if err != nil {
		return nil, "", out.Fail(out.ExitError, "CONFIG_ERROR", err.Error(), false)
	}
	src, err := auth.TokenSource(ctx, cfg)
	if err != nil {
		return nil, "", out.Fail(out.ExitAuthRequired, "AUTH_REQUIRED",
			err.Error()+" (run `docket auth login` first)", false)
	}
	client, err := cal.NewClient(ctx, src)
	if err != nil {
		return nil, "", out.Fail(out.ExitError, "CALENDAR_CLIENT_ERROR", err.Error(), false)
	}

	resolvedID := calendarID
	if calendarID == cal.PrimaryCalendar {
		who, err := auth.WhoAmIFromToken(ctx, src)
		if err != nil {
			return nil, "", out.Fail(out.ExitAuthRequired, "AUTH_EXPIRED", err.Error(), true)
		}
		resolvedID = cal.ResolveCalendarID(calendarID, who.Email)
	}

	return client, resolvedID, out.ExitOK
}

// calTarget resolves a --calendar value to a CalDAV client plus either a
// concrete resolved calendar id or, for the special value "all", the
// account's home id (its email address, resolved via primary) from which
// every calendar can be enumerated (see cal.ListCalendars).
func calTarget(ctx context.Context, calendarID string) (*caldav.Client, string, bool, int) {
	if calendarID == "all" {
		client, homeID, code := calContext(ctx, cal.PrimaryCalendar)
		return client, homeID, true, code
	}
	client, id, code := calContext(ctx, calendarID)
	return client, id, false, code
}

// requireTZ resolves --tz. Unlike the REST API, Google's CalDAV interface
// has no cheap way to ask a calendar its configured timezone, so --tz is
// required rather than defaulted — silently falling back to the server's
// local zone is exactly the off-by-hours bug docket-design.md §5 warns
// against.
func requireTZ(tzFlag, usage string) (*time.Location, int) {
	if tzFlag == "" {
		return nil, usageError(
			"--tz is required (Google's CalDAV interface can't cheaply report a "+
				"calendar's configured timezone, so it must be given explicitly)",
			usage+" --tz <IANA timezone, e.g. Australia/Melbourne>")
	}
	loc, err := time.LoadLocation(tzFlag)
	if err != nil {
		return nil, usageError(
			fmt.Sprintf("unrecognized --tz %q", tzFlag),
			usage+" --tz <IANA timezone, e.g. Australia/Melbourne>")
	}
	return loc, out.ExitOK
}

func cmdCalAgenda(ctx context.Context, args []string) int {
	fs := newFlagSet("cal agenda")
	days := fs.Int("days", 7, "how many days ahead to list")
	calendarID := fs.String("calendar", cal.PrimaryCalendar, "calendar id (all = every calendar)")
	tz := fs.String("tz", "", "IANA timezone, e.g. Australia/Melbourne")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket cal agenda [--days 7] [--calendar primary|all] --tz <zone>")
	}
	if *days <= 0 {
		return usageError("--days must be positive",
			"docket cal agenda [--days 7] [--calendar primary|all] --tz <zone>")
	}
	loc, code := requireTZ(*tz, "docket cal agenda [--days 7] [--calendar primary|all]")
	if code != out.ExitOK {
		return code
	}

	client, target, allCalendars, code := calTarget(ctx, *calendarID)
	if code != out.ExitOK {
		return code
	}

	now := time.Now().In(loc)
	var events []cal.Event
	var err error
	if allCalendars {
		events, err = cal.AgendaAll(ctx, client, target, now, now.AddDate(0, 0, *days), loc)
	} else {
		events, err = cal.Agenda(ctx, client, target, now, now.AddDate(0, 0, *days), loc)
	}
	if err != nil {
		return out.Fail(out.ExitError, "CALENDAR_API_ERROR", err.Error(), true)
	}
	return out.Emit(events)
}

func cmdCalShow(ctx context.Context, args []string) int {
	fs := newFlagSet("cal show")
	id := fs.String("id", "", "event id, from an agenda/find-slot result")
	calendarID := fs.String("calendar", cal.PrimaryCalendar, "calendar id")
	tz := fs.String("tz", "", "IANA timezone, e.g. Australia/Melbourne")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket cal show --id <event-id> [--calendar primary] --tz <zone>")
	}
	if *id == "" {
		return usageError("--id is required and must not be empty",
			"docket cal show --id <event-id> [--calendar primary] --tz <zone>; event ids come "+
				"from `cal agenda` output, in the form \"<uid>::<RFC3339 start time>\"")
	}
	loc, code := requireTZ(*tz, "docket cal show --id <event-id> [--calendar primary]")
	if code != out.ExitOK {
		return code
	}

	client, calendarIDResolved, code := calContext(ctx, *calendarID)
	if code != out.ExitOK {
		return code
	}

	event, err := cal.Show(ctx, client, calendarIDResolved, *id, loc)
	if err != nil {
		return out.Fail(out.ExitNotFound, "EVENT_NOT_FOUND", err.Error(), false)
	}
	return out.Emit(event)
}

func cmdCalFreeBusy(ctx context.Context, args []string) int {
	fs := newFlagSet("cal freebusy")
	start := fs.String("start", "", "start of the window, e.g. \"now\" or RFC3339")
	end := fs.String("end", "", "end of the window, e.g. \"+3d\" or RFC3339")
	calendarID := fs.String("calendar", cal.PrimaryCalendar, "calendar id (all = every calendar)")
	tz := fs.String("tz", "", "IANA timezone, e.g. Australia/Melbourne")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket cal freebusy --start ... --end ... [--calendar primary|all] --tz <zone>")
	}
	if *start == "" || *end == "" {
		return usageError("--start and --end are both required",
			"docket cal freebusy --start now --end +3d [--calendar primary|all] --tz <zone>")
	}
	loc, code := requireTZ(*tz, "docket cal freebusy --start ... --end ... [--calendar primary|all]")
	if code != out.ExitOK {
		return code
	}

	client, target, allCalendars, code := calTarget(ctx, *calendarID)
	if code != out.ExitOK {
		return code
	}

	startT, err := cal.ParseTime(*start, loc)
	if err != nil {
		return usageError(err.Error(), "docket cal freebusy --start ... --end ...")
	}
	endT, err := cal.ParseTime(*end, loc)
	if err != nil {
		return usageError(err.Error(), "docket cal freebusy --start ... --end ...")
	}

	ids := []string{target}
	if allCalendars {
		cals, err := cal.ListCalendars(ctx, client, target)
		if err != nil {
			return out.Fail(out.ExitError, "CALENDAR_API_ERROR", err.Error(), true)
		}
		ids = make([]string, 0, len(cals))
		for _, c := range cals {
			ids = append(ids, c.ID)
		}
	}

	fb, err := cal.FreeBusy(ctx, client, ids, startT, endT, loc)
	if err != nil {
		return out.Fail(out.ExitError, "CALENDAR_API_ERROR", err.Error(), true)
	}
	return out.Emit(map[string]any{
		"resolved_start": startT.Format(time.RFC3339),
		"resolved_end":   endT.Format(time.RFC3339),
		"busy":           fb.Busy,
	})
}

func cmdCalFindSlot(ctx context.Context, args []string) int {
	fs := newFlagSet("cal find-slot")
	duration := fs.String("duration", "", "minimum slot length, e.g. \"45m\"")
	within := fs.String("within", "5d", "how far ahead to search, e.g. \"5d\"")
	hours := fs.String("hours", "09:00-17:00", "daily search window, e.g. \"09:00-17:00\"")
	calendarID := fs.String("calendar", cal.PrimaryCalendar, "calendar id (all = across every calendar)")
	tz := fs.String("tz", "", "IANA timezone, e.g. Australia/Melbourne")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(),
			"docket cal find-slot --duration 45m --within 5d --hours 09:00-17:00 [--calendar primary|all] --tz <zone>")
	}
	if *duration == "" {
		return usageError("--duration is required, e.g. \"--duration 45m\"",
			"docket cal find-slot --duration 45m --within 5d --hours 09:00-17:00 [--calendar primary|all] --tz <zone>")
	}
	loc, code := requireTZ(*tz, "docket cal find-slot --duration 45m --within 5d --hours 09:00-17:00")
	if code != out.ExitOK {
		return code
	}

	client, target, allCalendars, code := calTarget(ctx, *calendarID)
	if code != out.ExitOK {
		return code
	}

	dur, err := cal.ParseDuration(*duration)
	if err != nil {
		return usageError(err.Error(), "docket cal find-slot --duration 45m ...")
	}
	withinDur, err := cal.ParseDuration(*within)
	if err != nil {
		return usageError(err.Error(), "docket cal find-slot --within 5d ...")
	}
	hourRange, err := cal.ParseHourRange(*hours)
	if err != nil {
		return usageError(err.Error(), "docket cal find-slot --hours 09:00-17:00 ...")
	}

	now := time.Now().In(loc)
	ids := []string{target}
	if allCalendars {
		cals, err := cal.ListCalendars(ctx, client, target)
		if err != nil {
			return out.Fail(out.ExitError, "CALENDAR_API_ERROR", err.Error(), true)
		}
		ids = make([]string, 0, len(cals))
		for _, c := range cals {
			ids = append(ids, c.ID)
		}
	}
	slots, err := cal.FindSlot(ctx, client, ids, dur, now, now.Add(withinDur), hourRange, loc)
	if err != nil {
		return out.Fail(out.ExitError, "CALENDAR_API_ERROR", err.Error(), true)
	}
	return out.Emit(slots)
}

func cmdCalCreate(ctx context.Context, args []string) int {
	fs := newFlagSet("cal create")
	summary := fs.String("summary", "", "event title")
	start := fs.String("start", "", "start time, e.g. \"tomorrow 14:00\" or RFC3339")
	duration := fs.String("duration", "", "event length, e.g. \"45m\"")
	location := fs.String("location", "", "location (optional)")
	attendees := fs.String("attendees", "", "comma-separated attendee email addresses (optional)")
	rruleFlag := fs.String("rrule", "",
		"RFC 5545 recurrence rule, e.g. \"FREQ=WEEKLY;BYDAY=MO,WE,FR\" (optional; makes the event recurring)")
	calendarID := fs.String("calendar", calDefaultCalendar(), "calendar id")
	tz := fs.String("tz", "", "IANA timezone, e.g. Australia/Melbourne")
	idempotencyKey := fs.String("idempotency-key", "",
		"reuse the same value on retry so a resend doesn't create a duplicate event")
	confirm := fs.Bool("confirm", false, "actually create (required)")
	dryRun := fs.Bool("dry-run", false, "preview without creating")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(),
			"docket cal create --summary ... --start ... --duration ... --tz <zone> [--rrule ...] [--confirm]")
	}
	if *summary == "" || *start == "" || *duration == "" {
		return usageError("--summary, --start, and --duration are all required",
			"docket cal create --summary ... --start ... --duration ... --tz <zone> [--confirm]")
	}
	loc, code := requireTZ(*tz, "docket cal create --summary ... --start ... --duration ...")
	if code != out.ExitOK {
		return code
	}

	startT, err := cal.ParseTime(*start, loc)
	if err != nil {
		return usageError(err.Error(), "docket cal create --start ...")
	}
	startT = startT.In(loc)
	dur, err := cal.ParseDuration(*duration)
	if err != nil {
		return usageError(err.Error(), "docket cal create --duration 45m ...")
	}
	endT := startT.Add(dur)

	var rr *rrule.ROption
	if *rruleFlag != "" {
		rr, err = cal.ParseRRule(*rruleFlag)
		if err != nil {
			return usageError(err.Error(), "docket cal create --rrule \"FREQ=WEEKLY;BYDAY=MO,WE,FR\" ...")
		}
	}

	client, calendarIDResolved, code := calContext(ctx, *calendarID)
	if code != out.ExitOK {
		return code
	}

	plan, err := cal.PrepareCreate(calendarIDResolved, *summary, startT, endT, false,
		*location, splitCommaList(*attendees), rr, *rruleFlag, *idempotencyKey)
	if err != nil {
		return usageError(err.Error(), "docket cal create --summary ... --start ... --duration ...")
	}

	rerun := fmt.Sprintf("docket cal create --summary %q --start %q --duration %s --calendar %s --tz %s",
		*summary, *start, *duration, *calendarID, *tz)
	rerun = withOptionalFlag(rerun, "location", *location)
	rerun = withOptionalFlag(rerun, "attendees", *attendees)
	rerun = withOptionalFlag(rerun, "rrule", *rruleFlag)
	rerun = withOptionalFlag(rerun, "idempotency-key", *idempotencyKey)
	rerun += " --confirm"
	proceed, code := writeGate("cal", "DOCKET_CAL_READONLY", *confirm, *dryRun, plan, rerun)
	if !proceed {
		return code
	}

	ev, err := plan.Execute(ctx, client)
	if err != nil {
		return out.Fail(out.ExitError, "CALENDAR_API_ERROR", err.Error(), true)
	}
	return out.Emit(ev)
}

func cmdCalUpdate(ctx context.Context, args []string) int {
	fs := newFlagSet("cal update")
	id := fs.String("id", "", "event id, from `cal agenda` output")
	calendarID := fs.String("calendar", calDefaultCalendar(), "calendar id")
	tz := fs.String("tz", "", "IANA timezone, e.g. Australia/Melbourne")
	summary := fs.String("summary", "", "new title (optional)")
	start := fs.String("start", "", "new start time (optional; requires --duration too)")
	duration := fs.String("duration", "", "new duration (optional; requires --start too)")
	location := fs.String("location", "", "new location (optional)")
	confirm := fs.Bool("confirm", false, "actually update (required)")
	dryRun := fs.Bool("dry-run", false, "preview without updating")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(),
			"docket cal update --id <event-id> [--summary ...] [--start ... --duration ...] [--location ...] --tz <zone> [--confirm]")
	}
	if *id == "" {
		return usageError("--id is required and must not be empty",
			"docket cal update --id <event-id> --tz <zone> [--summary ...] [--confirm]")
	}
	if (*start == "") != (*duration == "") {
		return usageError("--start and --duration must be given together",
			"docket cal update --id <event-id> --start ... --duration ... --tz <zone> [--confirm]")
	}
	if *summary == "" && *start == "" && *location == "" {
		return usageError("at least one of --summary, --start (with --duration), or --location is required",
			"docket cal update --id <event-id> --tz <zone> [--summary ...] [--start ... --duration ...] [--location ...] [--confirm]")
	}
	loc, code := requireTZ(*tz, "docket cal update --id <event-id>")
	if code != out.ExitOK {
		return code
	}

	client, calendarIDResolved, code := calContext(ctx, *calendarID)
	if code != out.ExitOK {
		return code
	}

	var summaryPtr, locationPtr *string
	if *summary != "" {
		summaryPtr = summary
	}
	if *location != "" {
		locationPtr = location
	}
	var startPtr, endPtr *time.Time
	if *start != "" {
		startT, err := cal.ParseTime(*start, loc)
		if err != nil {
			return usageError(err.Error(), "docket cal update --start ...")
		}
		startT = startT.In(loc)
		dur, err := cal.ParseDuration(*duration)
		if err != nil {
			return usageError(err.Error(), "docket cal update --duration 45m ...")
		}
		endT := startT.Add(dur)
		startPtr, endPtr = &startT, &endT
	}

	plan, err := cal.PrepareUpdate(ctx, client, calendarIDResolved, *id, loc, summaryPtr, startPtr, endPtr, false, locationPtr, calOwnEventsOnly())
	if err != nil {
		if errors.Is(err, cal.ErrNotOwned) {
			return out.Fail(out.ExitConfirmMissing, "NOT_OWNED", err.Error(), false)
		}
		return out.Fail(out.ExitNotFound, "EVENT_NOT_FOUND", err.Error(), false)
	}

	rerun := fmt.Sprintf("docket cal update --id %s --calendar %s --tz %s", *id, *calendarID, *tz)
	rerun = withOptionalFlag(rerun, "summary", *summary)
	rerun = withOptionalFlag(rerun, "location", *location)
	rerun = withOptionalFlag(rerun, "start", *start)
	rerun = withOptionalFlag(rerun, "duration", *duration)
	rerun += " --confirm"
	proceed, code := writeGate("cal", "DOCKET_CAL_READONLY", *confirm, *dryRun, plan, rerun)
	if !proceed {
		return code
	}

	ev, err := plan.Execute(ctx, client)
	if err != nil {
		return out.Fail(out.ExitError, "CALENDAR_API_ERROR", err.Error(), true)
	}
	return out.Emit(ev)
}

func cmdCalDelete(ctx context.Context, args []string) int {
	fs := newFlagSet("cal delete")
	id := fs.String("id", "", "event id, from `cal agenda` output")
	calendarID := fs.String("calendar", calDefaultCalendar(), "calendar id")
	tz := fs.String("tz", "", "IANA timezone, e.g. Australia/Melbourne")
	confirm := fs.Bool("confirm", false, "actually delete (required)")
	dryRun := fs.Bool("dry-run", false, "preview without deleting")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error(), "docket cal delete --id <event-id> --tz <zone> [--confirm]")
	}
	if *id == "" {
		return usageError("--id is required and must not be empty",
			"docket cal delete --id <event-id> --tz <zone> [--confirm]")
	}
	loc, code := requireTZ(*tz, "docket cal delete --id <event-id>")
	if code != out.ExitOK {
		return code
	}

	client, calendarIDResolved, code := calContext(ctx, *calendarID)
	if code != out.ExitOK {
		return code
	}

	plan, err := cal.PrepareDelete(ctx, client, calendarIDResolved, *id, loc, calOwnEventsOnly())
	if err != nil {
		if errors.Is(err, cal.ErrNotOwned) {
			return out.Fail(out.ExitConfirmMissing, "NOT_OWNED", err.Error(), false)
		}
		return out.Fail(out.ExitNotFound, "EVENT_NOT_FOUND", err.Error(), false)
	}

	rerun := fmt.Sprintf("docket cal delete --id %s --calendar %s --tz %s --confirm", *id, *calendarID, *tz)
	proceed, code := writeGate("cal", "DOCKET_CAL_READONLY", *confirm, *dryRun, plan, rerun)
	if !proceed {
		return code
	}

	if err := plan.Execute(ctx, client); err != nil {
		return out.Fail(out.ExitError, "CALENDAR_API_ERROR", err.Error(), true)
	}
	return out.Emit(map[string]any{"status": "deleted", "id": *id, "summary": plan.Summary})
}
