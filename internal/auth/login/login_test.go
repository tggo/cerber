package login

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tggo/cerber/internal/provider/mocks"

	"github.com/stretchr/testify/mock"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

func tokenResp() *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
		`{"access_token":"acc","refresh_token":"ref","expires_in":3600,"account":{"email_address":"u@e.com"}}`))}
}

func TestClaude_FullFlow(t *testing.T) {
	port := freePort(t)
	doer := mocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).Return(tokenResp(), nil)

	open := func(u string) error {
		pu, _ := url.Parse(u)
		state := pu.Query().Get("state")
		go func() {
			// Simulate the browser hitting the callback after authorization.
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=abc&state=%s", port, state))
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}

	tok, err := Claude(context.Background(), Options{
		Port: port, HTTP: doer, OpenURL: open, Out: io.Discard,
		Timeout: 5 * time.Second, Now: func() time.Time { return time.Unix(1000, 0) },
	})
	if err != nil {
		t.Fatalf("Claude: %v", err)
	}
	if tok.AccessToken != "acc" || tok.Email != "u@e.com" {
		t.Errorf("tokens = %+v", tok)
	}
}

func TestClaude_NoBrowserPrintsURL(t *testing.T) {
	port := freePort(t)
	doer := mocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).Return(tokenResp(), nil)
	var out strings.Builder

	// In no-browser mode, fire the callback ourselves once the server is up.
	go func() {
		time.Sleep(100 * time.Millisecond)
		// state is unknown here; in no-browser the server still accepts a matching
		// state only, so parse it from the printed URL.
		printed := out.String()
		i := strings.Index(printed, "state=")
		if i < 0 {
			return
		}
		state := strings.Fields(printed[i+6:])[0]
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=abc&state=%s", port, state))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	tok, err := Claude(context.Background(), Options{
		Port: port, NoBrowser: true, HTTP: doer, Out: &out, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Claude: %v", err)
	}
	if tok.AccessToken != "acc" {
		t.Errorf("token = %+v", tok)
	}
	if !strings.Contains(out.String(), "claude.ai/oauth/authorize") {
		t.Errorf("URL not printed: %s", out.String())
	}
}

func TestClaude_CallbackError(t *testing.T) {
	port := freePort(t)
	open := func(u string) error {
		go func() {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?error=access_denied", port))
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	_, err := Claude(context.Background(), Options{Port: port, OpenURL: open, Out: io.Discard, Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("expected authorization error")
	}
}

func TestClaude_Timeout(t *testing.T) {
	port := freePort(t)
	_, err := Claude(context.Background(), Options{
		Port: port, Out: io.Discard, OpenURL: func(string) error { return nil },
		Timeout: 50 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestClaude_ContextCancelled(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Claude(ctx, Options{Port: port, Out: io.Discard, OpenURL: func(string) error { return nil }, Timeout: time.Second}); err == nil {
		t.Fatal("expected context error")
	}
}

func TestClaude_PortInUse(t *testing.T) {
	port := freePort(t)
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := Claude(context.Background(), Options{Port: port, Out: io.Discard, OpenURL: func(string) error { return nil }}); err == nil {
		t.Fatal("expected port-in-use error")
	}
}
