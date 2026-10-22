package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/zachpmanson/docket/gmail/auth"
	"github.com/zachpmanson/docket/internal/out"
)

type policyContextKey struct{}

type commandPolicy struct {
	config    *auth.Config
	operation string
	accounts  []string // preflighted snapshot for auth accounts/list
}

func configFromContext(ctx context.Context) (*auth.Config, error) {
	policy, ok := ctx.Value(policyContextKey{}).(*commandPolicy)
	if !ok {
		return nil, fmt.Errorf("command has not passed permission preflight")
	}
	return policy.config, nil
}

func permissionFailure(err error) int {
	if denial, ok := err.(*auth.PermissionError); ok {
		return out.Fail(out.ExitConfirmMissing, denial.Code, denial.Error(), false)
	}
	return out.Fail(out.ExitUsage, "CONFIG_ERROR", err.Error(), false)
}

// authorizeCommand is called by all three domain dispatchers, before entering
// any handler. Unknown commands stay usage errors and cannot reach a handler.
// Only config and account-name metadata/stat discovery occur before authorization;
// token contents, migration, body files, stdin and network are all downstream.
func authorizeCommand(ctx context.Context, domain string, args []string) (context.Context, int) {
	if len(args) == 0 {
		return ctx, out.ExitOK // dispatcher's usage error
	}
	operation := auth.CanonicalOperation(domain, args[0])
	if !auth.KnownOperation(operation) {
		// Fail closed even if a future handler is added without a policy entry.
		var commands []string
		for _, name := range auth.OperationNames() {
			if strings.HasPrefix(name, domain+".") {
				commands = append(commands, strings.TrimPrefix(name, domain+"."))
			}
		}
		return ctx, usageError(fmt.Sprintf("unknown %s subcommand %q", domain, args[0]),
			"docket "+domain+" <"+strings.Join(commands, "|")+"> [flags]")
	}
	requested := accountFromContext(ctx)
	if domain == "auth" && (args[0] == "add" || args[0] == "remove") && requested == "" {
		return ctx, usageError("auth "+args[0]+" requires --account <name>", "docket auth "+args[0]+" --account <name>")
	}
	cfg, err := auth.LoadConfig()
	if err != nil {
		return ctx, permissionFailure(err)
	}
	policy := &commandPolicy{config: cfg, operation: operation}
	var account string
	switch operation {
	case "auth.accounts":
		if requested == "" {
			policy.accounts, err = auth.Accounts()
		} else {
			account, err = auth.ResolveAccount(requested)
			policy.accounts = []string{account}
		}
	case "auth.login", "auth.import":
		account, err = auth.LoginAccount(requested)
	default:
		account, err = auth.ResolveAccount(requested)
	}
	if err != nil {
		return ctx, out.Fail(out.ExitUsage, "ACCOUNT_SELECTION", err.Error(), false)
	}
	if operation == "auth.accounts" {
		// All-or-nothing: a denied profile must not be probed or leak its email
		// through a partial listing. --account selects a single permitted profile.
		check := policy.accounts
		if len(check) == 0 {
			check = []string{"default"}
		}
		for _, name := range check {
			if err := cfg.CheckPermission(name, operation, nil); err != nil {
				return ctx, permissionFailure(err)
			}
		}
	} else if err := cfg.CheckPermission(account, operation, nil); err != nil {
		return ctx, permissionFailure(err)
	}
	ctx = context.WithValue(ctx, accountContextKey{}, account)
	return context.WithValue(ctx, policyContextKey{}, policy), out.ExitOK
}
