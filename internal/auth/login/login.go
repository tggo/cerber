// Package login runs the interactive Claude Code OAuth flow: it starts a local
// callback server, sends the user to the authorization URL (opening a browser
// unless disabled), waits for the redirect, and exchanges the code for tokens.
package login

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/tggo/cerber/internal/auth/claude"
	"github.com/tggo/cerber/internal/provider"
)

// Options configures a Claude login.
type Options struct {
	Port      int                // callback port (default 54545)
	NoBrowser bool               // print the URL instead of opening a browser
	HTTP      provider.HTTPDoer  // token-exchange client (default http.DefaultClient)
	OpenURL   func(string) error // browser opener (default OS opener); injectable for tests
	Out       io.Writer          // user-facing messages
	Now       func() time.Time   // clock (for token expiry)
	Timeout   time.Duration      // how long to wait for the callback (default 5m)
}

type callback struct {
	code  string
	state string
	err   string
}

// Claude runs the OAuth flow and returns the obtained tokens.
func Claude(ctx context.Context, opt Options) (claude.Tokens, error) {
	if opt.Port == 0 {
		opt.Port = claude.DefaultCallbackPort
	}
	if opt.HTTP == nil {
		opt.HTTP = http.DefaultClient
	}
	if opt.OpenURL == nil {
		opt.OpenURL = openBrowser
	}
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.Timeout == 0 {
		opt.Timeout = 5 * time.Minute
	}

	pkce, err := claude.NewPKCE()
	if err != nil {
		return claude.Tokens{}, err
	}
	state, err := claude.NewState()
	if err != nil {
		return claude.Tokens{}, err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opt.Port))
	if err != nil {
		return claude.Tokens{}, fmt.Errorf("login: callback port %d unavailable: %w", opt.Port, err)
	}

	results := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cb := callback{code: q.Get("code"), state: q.Get("state"), err: q.Get("error")}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if cb.err != "" || cb.code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "<h1>cerber: login failed</h1><p>You can close this tab.</p>")
		} else {
			_, _ = io.WriteString(w, "<h1>cerber: login successful</h1><p>You can close this tab and return to the terminal.</p>")
		}
		select {
		case results <- cb:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	authURL := claude.BuildAuthURL(state, pkce, opt.Port)
	if opt.NoBrowser {
		fmt.Fprintf(opt.Out, "Open this URL in your browser to authorize cerber:\n\n  %s\n\n", authURL)
	} else {
		fmt.Fprintf(opt.Out, "Opening your browser to authorize cerber...\nIf it doesn't open, visit:\n\n  %s\n\n", authURL)
		if err := opt.OpenURL(authURL); err != nil {
			fmt.Fprintf(opt.Out, "(could not open browser automatically: %v)\n", err)
		}
	}

	select {
	case <-ctx.Done():
		return claude.Tokens{}, ctx.Err()
	case <-time.After(opt.Timeout):
		return claude.Tokens{}, fmt.Errorf("login: timed out waiting for callback after %s", opt.Timeout)
	case cb := <-results:
		if cb.err != "" {
			return claude.Tokens{}, fmt.Errorf("login: authorization error: %s", cb.err)
		}
		if cb.code == "" {
			return claude.Tokens{}, fmt.Errorf("login: no authorization code received")
		}
		if cb.state != "" && cb.state != state {
			return claude.Tokens{}, fmt.Errorf("login: state mismatch (possible CSRF)")
		}
		return claude.Exchange(ctx, opt.HTTP, cb.code, state, pkce.Verifier, opt.Port, opt.Now)
	}
}

// openBrowser opens url in the default browser for the current OS.
func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd = "xdg-open"
		args = []string{url}
	}
	return exec.Command(cmd, args...).Start()
}
