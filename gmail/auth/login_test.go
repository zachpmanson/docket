package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// closedLoginListener exercises the login flow without opening any sockets.
// Accept blocks like a real listener until shutdown closes it.
type closedLoginListener struct {
	done chan struct{}
	once sync.Once
}

func (l *closedLoginListener) Accept() (net.Conn, error) {
	<-l.done
	return nil, net.ErrClosed
}
func (l *closedLoginListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}
func (l *closedLoginListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8081}
}

func TestLoginPortListenerRedirectAndTunnel(t *testing.T) {
	if DefaultLoginPort != 8080 {
		t.Fatalf("default port = %d, want 8080", DefaultLoginPort)
	}
	for _, port := range []int{DefaultLoginPort, 8081, 1, 65535} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel() // print instructions, then exit without an OAuth exchange
			listener := &closedLoginListener{done: make(chan struct{})}
			var output bytes.Buffer
			calls := 0
			listen := func(network, address string) (net.Listener, error) {
				calls++
				if network != "tcp" || address != fmt.Sprintf("127.0.0.1:%d", port) {
					t.Fatalf("listen(%q, %q) must use selected port on IPv4 loopback", network, address)
				}
				return listener, nil
			}
			p := testProvider()
			token, err := loginWithPort(ctx, p, port, listen, &output)
			if token != nil || !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("login = %v, %v; listen calls=%d", token, err, calls)
			}
			select {
			case <-listener.done:
			default:
				t.Fatal("listener not closed")
			}
			text := output.String()
			if !strings.Contains(text, fmt.Sprintf("ssh -L %d:localhost:%d <host>", port, port)) {
				t.Fatalf("incorrect SSH tunnel: %s", text)
			}
			redirect := fmt.Sprintf("http://localhost:%d", port)
			if !strings.Contains(text, "Waiting for redirect on "+redirect+" ...") {
				t.Fatalf("incorrect waiting message: %s", text)
			}
			var authURL *url.URL
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, p.AuthURL+"?") {
					authURL, err = url.Parse(line)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if authURL == nil || authURL.Query().Get("redirect_uri") != redirect {
				t.Fatalf("authorization redirect does not match listener: %s", text)
			}
			if p.RedirectURI != "http://localhost" {
				t.Fatal("provider was modified")
			}
		})
	}
}

func TestLoginPortValidationBeforeListen(t *testing.T) {
	for _, port := range []int{-1, 0, 65536} {
		var output bytes.Buffer
		listen := func(_, _ string) (net.Listener, error) {
			t.Fatal("invalid port reached listener")
			return nil, nil
		}
		if _, err := loginWithPort(t.Context(), testProvider(), port, listen, &output); err == nil {
			t.Errorf("port %d accepted", port)
		}
		if output.Len() != 0 {
			t.Errorf("instructions printed for invalid port: %s", output.String())
		}
	}
}

func TestLoginPortBindFailure(t *testing.T) {
	var output bytes.Buffer
	bindErr := errors.New("address already in use")
	listen := func(_, address string) (net.Listener, error) {
		if address != "127.0.0.1:8081" {
			t.Fatalf("unexpected address %q", address)
		}
		return nil, bindErr
	}
	_, err := loginWithPort(t.Context(), testProvider(), 8081, listen, &output)
	if !errors.Is(err, bindErr) || !strings.Contains(err.Error(), ":8081") || output.Len() != 0 {
		t.Fatalf("bind failure = %v, output=%s", err, output.String())
	}
}
