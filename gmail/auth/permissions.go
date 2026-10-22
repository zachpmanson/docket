package auth

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Permissions uses exact operation names, not wildcards. Account entries override
// defaults one operation at a time. Omitted reads are allowed for compatibility;
// omitted writes and credential-management operations are denied, even for legacy
// configs. Deployment kill switches and ReadOnlyAccounts are always upper bounds.
type Permissions struct {
	Defaults map[string]bool            `toml:"defaults"`
	Accounts map[string]map[string]bool `toml:"accounts"`
}

type operation struct {
	write      bool
	credential bool
}

var operations = map[string]operation{
	"mail.search": {}, "mail.list": {}, "mail.read": {}, "mail.thread": {}, "mail.attachment": {},
	"mail.send": {write: true}, "mail.reply": {write: true}, "mail.label": {write: true},
	"cal.list": {}, "cal.search": {}, "cal.agenda": {}, "cal.show": {}, "cal.freebusy": {}, "cal.find-slot": {},
	"cal.create": {write: true}, "cal.update": {write: true}, "cal.delete": {write: true},
	"auth.whoami": {}, "auth.accounts": {},
	"auth.login": {credential: true}, "auth.import": {credential: true},
	"auth.export": {credential: true}, "auth.remove": {credential: true},
}

// CanonicalOperation maps CLI aliases to the permission they share.
func CanonicalOperation(domain, command string) string {
	if domain == "auth" {
		switch command {
		case "add":
			command = "login"
		case "list":
			command = "accounts"
		}
	}
	return domain + "." + command
}

func KnownOperation(name string) bool {
	_, ok := operations[name]
	return ok
}

// OperationNames returns the complete schema in stable order.
func OperationNames() []string {
	names := make([]string, 0, len(operations))
	for name := range operations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (p Permissions) validate() error {
	validate := func(values map[string]bool) error {
		for name := range values {
			if !KnownOperation(name) {
				return fmt.Errorf("unknown permission %q; valid operations: %s", name, strings.Join(OperationNames(), ", "))
			}
		}
		return nil
	}
	if err := validate(p.Defaults); err != nil {
		return fmt.Errorf("permissions.defaults: %w", err)
	}
	for account, values := range p.Accounts {
		if err := ValidateAccountName(account); err != nil {
			return fmt.Errorf("permissions.accounts: %w", err)
		}
		if err := validate(values); err != nil {
			return fmt.Errorf("permissions.accounts.%s: %w", account, err)
		}
	}
	return nil
}

// PermissionError is a policy refusal, never an auth/network error. Code remains
// WRITES_DISABLED for legacy upper bounds; operation denials use PERMISSION_DENIED.
type PermissionError struct {
	Account, Operation, Code, Reason string
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("account %q operation %q denied: %s; a human must review config.toml permissions and deployment policy (flags cannot override it)",
		e.Account, e.Operation, e.Reason)
}

// CheckPermission is the sole policy evaluator. It must run before token access,
// preview construction, body-file/stdin reads, or any network request. getenv is
// injected so precedence can be tested without modifying process state.
func (c *Config) CheckPermission(account, name string, getenv func(string) string) error {
	deny := func(code, reason string) error {
		return &PermissionError{Account: account, Operation: name, Code: code, Reason: reason}
	}
	op, known := operations[name]
	if !known {
		return deny("PERMISSION_DENIED", "unknown operation (fail closed)")
	}
	if err := ValidateAccountName(account); err != nil {
		return deny("PERMISSION_DENIED", err.Error())
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if op.credential && getenv("DOCKET_CAL_OWN_EVENTS_ONLY") != "" {
		return deny("WRITES_DISABLED", "DOCKET_CAL_OWN_EVENTS_ONLY blocks credential management to prevent bypassing event ownership")
	}
	if op.write || op.credential {
		for _, env := range []string{"DOCKET_READONLY", "DOCKET_MAIL_READONLY", "DOCKET_CAL_READONLY"} {
			applies := env == "DOCKET_READONLY" || op.credential ||
				(env == "DOCKET_MAIL_READONLY" && strings.HasPrefix(name, "mail.")) ||
				(env == "DOCKET_CAL_READONLY" && strings.HasPrefix(name, "cal."))
			if applies && getenv(env) != "" {
				return deny("WRITES_DISABLED", env+" is set")
			}
		}
		// Credential export/replacement/removal could escape account write policy.
		if c.AccountReadOnly(account) {
			return deny("WRITES_DISABLED", "read_only_accounts includes this account")
		}
	}
	allowed := !op.write && !op.credential
	if value, ok := c.Permissions.Defaults[name]; ok {
		allowed = value
	}
	if value, ok := c.Permissions.Accounts[account][name]; ok {
		allowed = value
	}
	if !allowed {
		return deny("PERMISSION_DENIED", "operation is disabled or has no explicit grant")
	}
	return nil
}
