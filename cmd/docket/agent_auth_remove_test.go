package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zachpmanson/docket/gmail/auth"
	"golang.org/x/oauth2"
)

func TestAgentConfirmedRemovalAllowsReenrollment(t *testing.T) {
	permissionSetup(t, agentAuthGrants)
	t.Setenv("DOCKET_AGENT_AUTH", "1")
	forbidNetwork(t)
	path, err := auth.AccountTokenPath("disposable")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("synthetic invalid credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := auth.RegisterAccount("disposable", ""); err != nil {
		t.Fatal(err)
	}
	before := snapshotAuthState(t)
	output, code := captureStdout(t, func() int { return run([]string{"auth", "remove", "--account=disposable"}) })
	if code != 6 || !strings.Contains(output, "CONFIRM_REQUIRED") {
		t.Fatalf("unconfirmed remove: %d %s", code, output)
	}
	if before[path] != snapshotAuthState(t)[path] {
		t.Fatal("unconfirmed remove changed invalid token")
	}
	output, code = captureStdout(t, func() int { return run([]string{"auth", "remove", "--account=disposable", "--confirm"}) })
	if code != 0 {
		t.Fatalf("confirmed remove: %d %s", code, output)
	}
	if _, err := os.Lstat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("profile directory not removed: %v", err)
	}
	if err := auth.CheckNewAccount("disposable"); err != nil {
		t.Fatalf("removed name cannot be reused: %v", err)
	}
	if err := auth.InstallNewAccount("disposable", &oauth2.Token{AccessToken: "synthetic-new-consent"}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRemovalNeverRecursivelyDeletesUnexpectedContents(t *testing.T) {
	permissionSetup(t, agentAuthGrants)
	t.Setenv("DOCKET_AGENT_AUTH", "1")
	forbidNetwork(t)
	path, err := auth.AccountTokenPath("work")
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(filepath.Dir(path), "unexpected")
	if err := os.WriteFile(sentinel, []byte("protected unrelated file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, code := captureStdout(t, func() int { return run([]string{"auth", "remove", "--account=work", "--confirm"}) })
	if code == 0 {
		t.Fatal("nonempty directory cleanup unexpectedly succeeded")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "protected unrelated file" {
		t.Fatal("removal deleted unexpected contents")
	}
	if err := auth.CheckNewAccount("work"); err == nil {
		t.Fatal("nonempty leftover directory accepted for enrollment")
	}
}
