package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zachpmanson/docket/gmail/auth"
	"github.com/zachpmanson/docket/gmail/mail"
	"github.com/zachpmanson/docket/internal/out"
)

func attachmentArgs(path string) []string {
	return []string{"mail", "attachment", "--account", "default", "--id", "m1", "--part", "1", "--out", path}
}

func attachmentStdoutArgs(extra ...string) []string {
	return append([]string{"mail", "attachment", "--account", "default", "--id", "m1", "--part", "1", "--stdout"}, extra...)
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
			assertDenial(t, attachmentStdoutArgs(), "default", "mail.attachment", "PERMISSION_DENIED")
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

func TestAttachmentStdoutPreflight(t *testing.T) {
	permissionSetup(t, "")
	forbidNetwork(t)
	t.Setenv("DOCKET_CAL_OWN_EVENTS_ONLY", "1")
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{attachmentStdoutArgs(), out.ExitError, "OUTPUT_WRITE_FAILED"}, // no operator opt-in
		{attachmentStdoutArgs("--out", "ignored"), out.ExitUsage, "USAGE_ERROR"},
		{attachmentStdoutArgs("--max-bytes", "-1"), out.ExitUsage, "USAGE_ERROR"},
		{attachmentStdoutArgs("extra"), out.ExitUsage, "USAGE_ERROR"},
	} {
		output, code := captureStdout(t, func() int { return run(tc.args) })
		var envelope out.Envelope
		if err := json.Unmarshal([]byte(output), &envelope); err != nil {
			t.Fatal(err)
		}
		if code != tc.code || envelope.OK || envelope.Error == nil || envelope.Error.Code != tc.want {
			t.Fatalf("%v: code=%d output=%s", tc.args, code, output)
		}
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
			// Use non-text bytes; the metadata deliberately understates their size.
			_, _ = io.WriteString(w, `{"id":"m1","payload":{"partId":"1","filename":"image.png","mimeType":"image/png","body":{"size":2,"data":"AP-AZQo"}}}`)
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
	want := "\x00\xff\x80e\n"
	result, code := captureStdout(t, func() int { return run(attachmentStdoutArgs()) })
	var reply struct {
		OK   bool
		Data mail.FetchedAttachment
	}
	if err := json.Unmarshal([]byte(result), &reply); err != nil {
		t.Fatal(err)
	}
	if code != out.ExitOK || !reply.OK || reply.Data.Content == nil || string(*reply.Data.Content) != want ||
		reply.Data.Size != int64(len(want)) || reply.Data.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(want))) ||
		reply.Data.Path != "" || strings.Contains(result, `"path"`) {
		t.Fatalf("stdout fetch: code=%d output=%s", code, result)
	}
	result, code = captureStdout(t, func() int { return run(attachmentStdoutArgs("--max-bytes", "3")) })
	if code != out.ExitError || !strings.Contains(result, `"ATTACHMENT_TOO_LARGE"`) || strings.Contains(result, `"content_base64"`) {
		t.Fatalf("stdout cap: code=%d output=%s", code, result)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("stdout mode changed the protected spool: %v %v", entries, err)
	}
	for _, path := range []string{"relative.png", filepath.Join(root, "absolute.png")} {
		output, code := captureStdout(t, func() int { return run(attachmentArgs(path)) })
		if code != out.ExitOK || strings.Contains(output, `"content_base64"`) {
			t.Fatalf("%s: %d %s", path, code, output)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("%q = %q %v", path, got, err)
		}
	}
}
