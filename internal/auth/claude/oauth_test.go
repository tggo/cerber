package claude

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tggo/cerber/internal/provider/mocks"

	"github.com/stretchr/testify/mock"
)

func TestNewPKCE(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if p.Verifier == "" || p.Challenge == "" {
		t.Fatal("empty pkce")
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); p.Challenge != want {
		t.Errorf("challenge != S256(verifier)")
	}
	// two calls differ
	p2, _ := NewPKCE()
	if p2.Verifier == p.Verifier {
		t.Error("pkce not random")
	}
}

func TestBuildAuthURL(t *testing.T) {
	u := BuildAuthURL("st4te", PKCE{Challenge: "chal"}, 54545)
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := pu.Query()
	if q.Get("client_id") != ClientID || q.Get("state") != "st4te" ||
		q.Get("code_challenge") != "chal" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != "http://localhost:54545/callback" {
		t.Errorf("bad auth url params: %v", q)
	}
}

func okExchange() *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
		`{"access_token":"acc","refresh_token":"ref","expires_in":3600,"account":{"email_address":"u@e.com"}}`))}
}

func TestExchange_Success(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	var body []byte
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		body, _ = io.ReadAll(r.Body)
		return okExchange(), nil
	})
	tok, err := Exchange(context.Background(), doer, "the-code", "st", "verif", 54545,
		func() time.Time { return time.Unix(1000, 0) })
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "acc" || tok.RefreshToken != "ref" || tok.Email != "u@e.com" {
		t.Errorf("tokens = %+v", tok)
	}
	if !tok.ExpiresAt.Equal(time.Unix(1000, 0).Add(time.Hour)) {
		t.Errorf("expiry = %v", tok.ExpiresAt)
	}
	if captured.URL.String() != TokenURL {
		t.Errorf("url = %s", captured.URL)
	}
	if !strings.Contains(string(body), `"grant_type":"authorization_code"`) || !strings.Contains(string(body), `"code_verifier":"verif"`) {
		t.Errorf("body = %s", body)
	}
}

func TestExchange_CodeWithState(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var body []byte
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(r.Body)
		return okExchange(), nil
	})
	if _, err := Exchange(context.Background(), doer, "abc#embedded", "orig", "v", 1, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"code":"abc"`) || !strings.Contains(string(body), `"state":"embedded"`) {
		t.Errorf("code/state split wrong: %s", body)
	}
}

func TestExchange_Errors(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		d := mocks.NewHTTPDoer(t)
		d.EXPECT().Do(mock.Anything).Return(nil, errors.New("dial"))
		if _, err := Exchange(context.Background(), d, "c", "s", "v", 1, nil); err == nil {
			t.Fatal("want err")
		}
	})
	t.Run("non-200", func(t *testing.T) {
		d := mocks.NewHTTPDoer(t)
		d.EXPECT().Do(mock.Anything).Return(&http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`bad`))}, nil)
		if _, err := Exchange(context.Background(), d, "c", "s", "v", 1, nil); err == nil {
			t.Fatal("want err")
		}
	})
	t.Run("bad json", func(t *testing.T) {
		d := mocks.NewHTTPDoer(t)
		d.EXPECT().Do(mock.Anything).Return(&http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{`))}, nil)
		if _, err := Exchange(context.Background(), d, "c", "s", "v", 1, nil); err == nil {
			t.Fatal("want err")
		}
	})
	t.Run("missing access_token", func(t *testing.T) {
		d := mocks.NewHTTPDoer(t)
		d.EXPECT().Do(mock.Anything).Return(&http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"refresh_token":"r"}`))}, nil)
		if _, err := Exchange(context.Background(), d, "c", "s", "v", 1, nil); err == nil {
			t.Fatal("want err")
		}
	})
}
