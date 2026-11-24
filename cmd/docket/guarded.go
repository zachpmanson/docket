package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/zachpmanson/docket/gmail/mail"
	"github.com/zachpmanson/docket/internal/out"
)

const guardedSudo = "/run/wrappers/bin/sudo"

// Invoked by the Naboo shim as the original caller, never by the sudo rule.
// The sudo invocation receives only fetch arguments, never a destination.
func runGuarded(runner string, args []string) int {
	prefix := []string{"-n", "-u", "docket-agent", "--", runner}
	filtered, account, err := extractAccountFlag(args)
	if err == nil && len(filtered) >= 2 && filtered[0] == "mail" && filtered[1] == "attachment" {
		fs := newFlagSet("mail attachment")
		dir := fs.String("out-dir", "", "absolute caller-owned output directory")
		name := fs.String("out", "", "file inside the output directory")
		id := fs.String("id", "", "Gmail message ID")
		part := fs.String("part", "", "Gmail part ID")
		max := fs.Int("max-bytes", mail.DefaultAttachmentMaxBytes, "maximum decoded bytes (0 = no cap)")
		parseErr := fs.Parse(filtered[2:])
		if *dir != "" && parseErr == nil {
			if fs.NArg() != 0 || *name == "" || *id == "" || *part == "" || *max < 0 {
				return usageError("invalid caller attachment destination or fetch arguments",
					"docket mail attachment --id <gm-id> --part <part-id> --out-dir <absolute-dir> --out <file> [--max-bytes <n>]")
			}
			fetchArgs := append(prefix, "mail", "attachment", "--id="+*id, "--part="+*part, "--max-bytes="+strconv.Itoa(*max), "--stdout")
			if account != "" {
				fetchArgs = append(fetchArgs, "--account="+account)
			}
			cmd := exec.Command(guardedSudo, fetchArgs...)
			cmd.Stderr = os.Stderr
			return saveGuardedAttachment(*dir, *name, cmd.Output)
		}
	}
	// Preserve argv, stdio and signal handling for every existing command.
	if err := syscall.Exec(guardedSudo, append([]string{guardedSudo}, append(prefix, args...)...), os.Environ()); err != nil {
		return runnerFailure(err.Error())
	}
	return out.ExitError // Exec only returns on failure.
}

func saveGuardedAttachment(dir, name string, fetch func() ([]byte, error)) int {
	dest := mail.FetchOptions{OutputRoot: dir, OutPath: name}
	closer, err := dest.PrepareOutput()
	if err != nil {
		return failMailAttachment(err)
	}
	defer closer.Close()
	payload, fetchErr := fetch()
	var reply struct {
		OK    bool
		Data  mail.FetchedAttachment
		Error *out.Error
	}
	if err := json.Unmarshal(payload, &reply); err != nil {
		return runnerFailure("guarded attachment fetch did not return JSON")
	}
	if fetchErr != nil {
		var exit *exec.ExitError
		if !reply.OK && reply.Error != nil && errors.As(fetchErr, &exit) {
			return out.Fail(exit.ExitCode(), reply.Error.Code, reply.Error.Message, reply.Error.Retryable)
		}
		return runnerFailure("guarded attachment fetch failed")
	}
	attachment := &reply.Data
	if !reply.OK || reply.Error != nil || attachment.Content == nil || attachment.Path != "" ||
		attachment.Size != int64(len(*attachment.Content)) ||
		attachment.SHA256 != fmt.Sprintf("%x", sha256.Sum256(*attachment.Content)) {
		return runnerFailure("invalid attachment response from guarded fetch")
	}
	attachment.Path, err = dest.SaveContent(*attachment.Content)
	if err != nil {
		return failMailAttachment(err)
	}
	attachment.Content = nil
	return out.Emit(attachment)
}

func runnerFailure(message string) int {
	return out.Fail(out.ExitError, "GUARDED_RUNNER_FAILED", message, false)
}
