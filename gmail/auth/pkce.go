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

// LoginResult is what a completed interactive login produces.
type LoginResult struct {
	Token      *oauth2.Token
	AuthURL    string // printed to the user before the listener blocks
	ListenAddr string
}

// Login runs the PKCE authorization-code flow: it starts a loopback HTTP
// listener, returns the authorization URL to display, and blocks until the
// browser redirect completes or ctx is cancelled.
//
// On a headless box the caller is expected to print the SSH tunnel command
// (`ssh -L <port>:localhost:<port> <host>`) alongside AuthURL before the
// user opens it in a local browser. See docket-design.md §3.
func Login(ctx context.Context, p Provider) (*oauth2.Token, error) {
	verifier, challenge, err := newPKCEVerifier()
	if err != nil {
		return nil, fmt.Errorf("generating PKCE verifier: %w", err)
	}

	// Fixed port so the SSH tunnel command printed below stays valid for the
	// whole flow: `ssh -L loginPort:localhost:loginPort <host>`.
	const loginPort = 8080
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", loginPort))
	if err != nil {
		return nil, fmt.Errorf("starting loopback listener on :%d: %w", loginPort, err)
	}
	defer listener.Close()

	redirectURI := fmt.Sprintf("http://localhost:%d", loginPort)
	pCopy := p
	pCopy.RedirectURI = redirectURI
	cfg := oauthConfig(pCopy)

	state, _, err := newPKCEVerifier() // reuse as a random opaque state token
	if err != nil {
		return nil, err
	}

	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)

	type result struct {
		code string
		err  error
	}
	resultCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
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
	fmt.Println(authURL)
	fmt.Printf("\nWaiting for redirect on %s ...\n", redirectURI)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resultCh:
		if res.err != nil {
			return nil, res.err
		}
		exchangeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		tok, err := cfg.Exchange(exchangeCtx, res.code,
			oauth2.SetAuthURLParam("code_verifier", verifier))
		if err != nil {
			return nil, fmt.Errorf("exchanging code: %w", err)
		}
		return tok, nil
	}
}
