package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zachpmanson/docket/gmail/auth"
	"github.com/zachpmanson/docket/internal/out"
)

func attachmentArgs(path string) []string {
	return []string{"mail", "attachment", "--account", "default", "--id", "m1", "--part", "1", "--out", path}
}

func assertAttachmentOutputDenied(t *testing.T, path string) {
	t.Helper()
	output, code := captureStdout(t, func() int { return run(attachmentArgs(path)) })
	var envelope out.Envelope
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatal(err)
	}
	if code != out.ExitError || envelope.OK || envelope.Error == nil || envelope.Error.Code != "OUTPUT_WRITE_FAILED" || envelope.Error.Retryable {
		t.Fatalf("destination %q: code=%d output=%s", path, code, output)
	}
}

func TestAttachmentRestrictedDeploymentRequiresRootBeforeCredentials(t *testing.T) {
	for _, env := range []string{"DOCKET_READONLY", "DOCKET_MAIL_READONLY", "DOCKET_CAL_READONLY", "DOCKET_CAL_OWN_EVENTS_ONLY"} {
		t.Run(env, func(t *testing.T) {
			permissionSetup(t, "")
			t.Setenv(env, "0") // Any nonempty value restricts output.
			forbidNetwork(t)
			token, err := auth.AccountTokenPath("default")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(token, []byte("invalid token: must not be read"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "image.png")
			assertAttachmentOutputDenied(t, path)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("denial wrote output: %v", err)
			}
			// Normal permission precedence still wins over output preflight.
			config, err := auth.ConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, []byte("[provider]\nclient_id = \"test\"\n[permissions.defaults]\n\"mail.attachment\" = false\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			assertDenial(t, attachmentArgs(path), "default", "mail.attachment", "PERMISSION_DENIED")
		})
	}
}

func TestAttachmentInvalidDestinationsDoNotReadOrMigrateTokens(t *testing.T) {
	permissionSetup(t, "")
	forbidNetwork(t)
	root := t.TempDir()
	t.Setenv("DOCKET_ATTACHMENT_ROOT", root)
	t.Setenv("DOCKET_MAIL_READONLY", "1")
	config, err := auth.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(t.TempDir(), "policy.toml")
	if err := os.WriteFile(store, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, config); err != nil {
		t.Fatal(err)
	}
	token, err := auth.AccountTokenPath("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(os.Getenv("XDG_STATE_HOME"), "docket", "token.json")
	if err := os.WriteFile(legacy, []byte("invalid legacy token: must not be read or migrated"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := auth.RegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	beforeRegistry, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(token), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		config, registry, token, filepath.Join(filepath.Dir(token), "new-token.json"),
		filepath.Join(filepath.Dir(config), ".profile"), "../escape.png",
		"escape/token.json", "escape/new-token.json", "missing/image.png", ".", root,
	} {
		assertAttachmentOutputDenied(t, path)
	}
	if _, err := os.Stat(token); !os.IsNotExist(err) {
		t.Fatalf("invalid destination migrated token: %v", err)
	}
	if target, err := os.Readlink(config); err != nil || target != store {
		t.Fatalf("config symlink replaced: %q %v", target, err)
	}
	if got, err := os.ReadFile(store); err != nil || string(got) != string(policy) {
		t.Fatalf("policy changed: %q %v", got, err)
	}
	if got, err := os.ReadFile(registry); err != nil || string(got) != string(beforeRegistry) {
		t.Fatalf("registry changed: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(token), "new-token.json")); !os.IsNotExist(err) {
		t.Fatalf("new credential path created: %v", err)
	}
}

func TestAttachmentCLIRootedDownloads(t *testing.T) {
	permissionSetup(t, "")
	root := t.TempDir()
	t.Setenv("DOCKET_ATTACHMENT_ROOT", root)
	for _, env := range []string{"DOCKET_READONLY", "DOCKET_MAIL_READONLY", "DOCKET_CAL_READONLY", "DOCKET_CAL_OWN_EVENTS_ONLY"} {
		t.Setenv(env, "1")
	}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer default-token" || r.Method != "GET" {
			t.Errorf("unexpected fake Gmail request: %s %s %v", r.Method, r.URL, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/m1"):
			_, _ = io.WriteString(w, `{"id":"m1","payload":{"partId":"1","filename":"image.png","mimeType":"image/png","body":{"size":5,"data":"aW1hZ2U"}}}`)
		case strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = io.WriteString(w, `{"labels":[]}`)
		default:
			t.Errorf("unexpected fake Gmail path: %s", r.URL)
		}
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	for _, path := range []string{"relative.png", filepath.Join(root, "absolute.png")} {
		output, code := captureStdout(t, func() int { return run(attachmentArgs(path)) })
		if code != out.ExitOK {
			t.Fatalf("%s: %d %s", path, code, output)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "image" {
			t.Fatalf("%q = %q %v", path, got, err)
		}
	}
}
