package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// newPKCEVerifier returns a random RFC 7636 code_verifier and its S256
// code_challenge.
func newPKCEVerifier() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func oauthConfig(p Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		RedirectURL:  p.RedirectURI,
		Scopes:       p.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  p.AuthURL,
			TokenURL: p.TokenURL,
		},
	}
}

// Pending is a prepared authorization-code login: the authorization URL to
// send the user to, plus the state the caller must verify on the callback
// and the code verifier to exchange the returned code with. A process that
// serves its own HTTP (an embedded server) holds a Pending across the
// consent round-trip; the CLI closes the loop with a throwaway loopback
// listener (see Login).
type Pending struct {
	Config    *oauth2.Config
	State     string // verify the callback's state= against this
	Verifier  string // exchange the callback's code= with this
	Challenge string // S256 challenge embedded in the authorization URL
}

// AuthURL returns the URL the user's browser must be sent to. Callers that
// print it (the CLI) or redirect to it (a server) do so from here so the
// flow is exercised identically either way.
func (p *Pending) AuthURL() string {
	return p.Config.AuthCodeURL(p.State,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("code_challenge", p.Challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

// BeginLogin prepares a PKCE authorization-code login for a provider without
// binding a port or blocking: it returns a Pending carrying everything needed
// to finish. redirectURI names the URI that will actually receive the
// redirect — the CLI passes its loopback listener's URL, an embedded server
// passes its own callback URL (for the Thunderbird client docket uses, that
// must stay a pathless http://localhost:<port> — see docket-design.md §3).
func BeginLogin(p Provider, redirectURI string) (*Pending, error) {
	verifier, challenge, err := newPKCEVerifier()
	if err != nil {
		return nil, fmt.Errorf("generating PKCE verifier: %w", err)
	}
	// Reuse the verifier generator as a source of an opaque state token: it
	// is random and unguessable, which is all the state value needs.
	state, _, err := newPKCEVerifier()
	if err != nil {
		return nil, err
	}
	pCopy := p
	pCopy.RedirectURI = redirectURI
	return &Pending{
		Config:    oauthConfig(pCopy),
		State:     state,
		Verifier:  verifier,
		Challenge: challenge,
	}, nil
}

// ExchangeCode completes a flow started by BeginLogin once the provider
// redirects back with an authorization code. The caller is responsible for
// matching the callback's state= against Pending.State before calling this —
// the lib cannot know how the caller stashed the Pending, so it does not
// verify the state itself.
func ExchangeCode(ctx context.Context, p *Pending, code string) (*oauth2.Token, error) {
	exchangeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tok, err := p.Config.Exchange(exchangeCtx, code,
		oauth2.SetAuthURLParam("code_verifier", p.Verifier))
	if err != nil {
		return nil, fmt.Errorf("exchanging code: %w", err)
	}
	return tok, nil
}

// Login runs the complete interactive PKCE flow: it starts a loopback HTTP
// listener, returns the authorization URL to display, and blocks until the
// browser redirect completes or ctx is cancelled.
//
// On a headless box the caller is expected to print the SSH tunnel command
// (`ssh -L <port>:localhost:<port> <host>`) alongside AuthURL before the
// user opens it in a local browser. See docket-design.md §3.
func Login(ctx context.Context, p Provider) (*oauth2.Token, error) {
	// Fixed port so the SSH tunnel command printed below stays valid for the
	// whole flow: `ssh -L loginPort:localhost:loginPort <host>`.
	const loginPort = 8080
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", loginPort))
	if err != nil {
		return nil, fmt.Errorf("starting loopback listener on :%d: %w", loginPort, err)
	}
	defer listener.Close()

	redirectURI := fmt.Sprintf("http://localhost:%d", loginPort)
	pending, err := BeginLogin(p, redirectURI)
	if err != nil {
		return nil, err
	}

	type result struct {
		code string
		err  error
	}
	resultCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != pending.State {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			resultCh <- result{err: fmt.Errorf("oauth state mismatch")}
			return
		}
		if errMsg := q.Get("error"); errMsg != "" {
			http.Error(w, errMsg, http.StatusBadRequest)
			resultCh <- result{err: fmt.Errorf("authorization denied: %s", errMsg)}
			return
		}
		code := q.Get("code")
		fmt.Fprintln(w, "docket: login complete, you can close this tab.")
		resultCh <- result{code: code}
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	defer srv.Shutdown(context.Background())

	fmt.Printf("On a headless box, tunnel first:\n  ssh -L %d:localhost:%d <host>\n\n", loginPort, loginPort)
	fmt.Println("Then open this URL in a browser:")
	fmt.Println(pending.AuthURL())
	fmt.Printf("\nWaiting for redirect on %s ...\n", redirectURI)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resultCh:
		if res.err != nil {
			return nil, res.err
		}
		return ExchangeCode(ctx, pending, res.code)
	}
}
