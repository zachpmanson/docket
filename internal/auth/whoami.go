package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"
)

// WhoAmI is the shape returned by `docket auth whoami`.
type WhoAmI struct {
	Email   string `json:"email"`
	Expires string `json:"token_expires,omitempty"`
}

// TokenSource returns the persisting, flock-guarded token source for the
// current config and on-disk token file. Mail and calendar commands use this
// as the credential for their respective clients.
func TokenSource(ctx context.Context, cfg *Config) (oauth2.TokenSource, error) {
	path, err := TokenPath()
	if err != nil {
		return nil, err
	}
	tok, err := readToken(path)
	if err != nil {
		return nil, fmt.Errorf("no token on disk (run `docket auth login`): %w", err)
	}
	base := oauthConfig(cfg.Provider).TokenSource(ctx, tok)
	return NewPersistingTokenSource(base, path), nil
}

// WhoAmIFromToken calls Google's tokeninfo endpoint to resolve the email
// address associated with the current access token.
func WhoAmIFromToken(ctx context.Context, src oauth2.TokenSource) (*WhoAmI, error) {
	tok, err := src.Token()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.googleapis.com/oauth2/v3/tokeninfo?access_token="+tok.AccessToken, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tokeninfo returned %d: %s", resp.StatusCode, body)
	}

	var info struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}

	who := &WhoAmI{Email: info.Email}
	if !tok.Expiry.IsZero() {
		who.Expires = tok.Expiry.Format("2006-01-02T15:04:05Z07:00")
	}
	return who, nil
}

// ImportToken reads a JSON-encoded oauth2.Token from r (e.g. stdin) and
// persists it to the token store, so a login performed on one machine can be
// piped to a headless server. Refresh tokens aren't machine-bound.
func ImportToken(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return fmt.Errorf("parsing token: %w", err)
	}
	path, err := TokenPath()
	if err != nil {
		return err
	}
	return writeToken(path, &tok)
}

// ExportToken writes the current on-disk token as JSON to w (e.g. stdout),
// for piping to `docket auth import` on another machine.
func ExportToken(w io.Writer) error {
	path, err := TokenPath()
	if err != nil {
		return err
	}
	tok, err := readToken(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(tok)
}
