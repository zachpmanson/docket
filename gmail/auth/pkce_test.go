package auth

import (
	"net/url"
	"testing"
)

func testProvider() Provider {
	return Provider{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		AuthURL:      "https://example.com/oauth/authorize",
		TokenURL:     "https://example.com/oauth/token",
		RedirectURI:  "http://localhost",
		Scopes:       []string{"mail.read", "calendar"},
		UsePKCE:      true,
	}
}

func TestBeginLoginBuildsAUrlAndKeepsTheSecrets(t *testing.T) {
	pending, err := BeginLogin(testProvider(), "http://localhost:8080")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	u, err := url.Parse(pending.AuthURL())
	if err != nil {
		t.Fatalf("parsing AuthURL: %v", err)
	}
	if u.Scheme != "https" || u.Host != "example.com" {
		t.Errorf("authorize URL is provider-scoped, got %s://%s", u.Scheme, u.Host)
	}
	q := u.Query()
	if q.Get("response_type") != "code" {
		t.Errorf("response_type=code, got %q", q.Get("response_type"))
	}
	if q.Get("client_id") != "test-client" {
		t.Errorf("client_id carried through, got %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "http://localhost:8080" {
		t.Errorf("redirect_uri honours the BeginLogin argument, got %q", q.Get("redirect_uri"))
	}
	if q.Get("scope") != "mail.read calendar" {
		t.Errorf("scopes space-joined, got %q", q.Get("scope"))
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Errorf("PKCE S256 challenge in the URL, got method=%q challenge=%q",
			q.Get("code_challenge_method"), q.Get("code_challenge"))
	}
	state := q.Get("state")
	if state == "" || state == pending.Verifier {
		t.Errorf("opaque state in the URL, distinct from the verifier (state=%q)", state)
	}
	if pending.State != state {
		t.Errorf("Pending.State matches the URL state")
	}
}

func TestBeginLoginUsesTheCallerRedirectUri(t *testing.T) {
	// A service names its own callback URI; the configured default must not
	// leak through.
	pending, err := BeginLogin(testProvider(), "http://localhost:8765")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	u, err := url.Parse(pending.AuthURL())
	if err != nil {
		t.Fatalf("parsing AuthURL: %v", err)
	}
	if got := u.Query().Get("redirect_uri"); got != "http://localhost:8765" {
		t.Errorf("server-provided redirect_uri wins over the config default, got %q", got)
	}
	if pending.Config.RedirectURL != "http://localhost:8765" {
		t.Errorf("Pending.Config points at the caller's redirect URI")
	}
}

func TestBeginLoginIsRandomPerCall(t *testing.T) {
	a, _ := BeginLogin(testProvider(), "http://localhost:8080")
	b, _ := BeginLogin(testProvider(), "http://localhost:8080")
	if a.State == b.State {
		t.Errorf("state is random per call (both %q)", a.State)
	}
	if a.Verifier == b.Verifier {
		t.Errorf("verifier is random per call")
	}
	if a.AuthURL() == b.AuthURL() {
		t.Errorf("authorization URLs differ per call")
	}
}