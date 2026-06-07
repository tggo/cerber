package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cerber/internal/provider/mocks"

	"github.com/stretchr/testify/mock"
)

func jsonResp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func fixedRefresher(t *testing.T, doer *mocks.HTTPDoer) *Refresher {
	return NewRefresher("https://api.anthropic.com", doer,
		WithRefresherClock(func() time.Time { return time.Unix(1000, 0) }))
}

func TestRefresh_Success(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return jsonResp(200, `{"access_token":"new-acc","refresh_token":"new-ref","expires_in":3600}`), nil
	})
	tok, err := fixedRefresher(t, doer).Refresh(context.Background(), "old-ref")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.AccessToken != "new-acc" || tok.RefreshToken != "new-ref" {
		t.Errorf("tokens: %+v", tok)
	}
	if !tok.ExpiresAt.Equal(time.Unix(1000, 0).Add(time.Hour)) {
		t.Errorf("expiry: %v", tok.ExpiresAt)
	}
	if captured.URL.String() != "https://api.anthropic.com/v1/oauth/token" {
		t.Errorf("url: %s", captured.URL)
	}
	b, _ := io.ReadAll(captured.Body)
	if !strings.Contains(string(b), `"grant_type":"refresh_token"`) || !strings.Contains(string(b), ClaudeCodeClientID) {
		t.Errorf("body: %s", b)
	}
}

func TestRefresh_Errors(t *testing.T) {
	t.Run("empty refresh token", func(t *testing.T) {
		if _, err := fixedRefresher(t, mocks.NewHTTPDoer(t)).Refresh(context.Background(), ""); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("transport error", func(t *testing.T) {
		d := mocks.NewHTTPDoer(t)
		d.EXPECT().Do(mock.Anything).Return(nil, errors.New("dial"))
		if _, err := fixedRefresher(t, d).Refresh(context.Background(), "r"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("non-200", func(t *testing.T) {
		d := mocks.NewHTTPDoer(t)
		d.EXPECT().Do(mock.Anything).Return(jsonResp(401, `{"error":"bad"}`), nil)
		if _, err := fixedRefresher(t, d).Refresh(context.Background(), "r"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("bad json", func(t *testing.T) {
		d := mocks.NewHTTPDoer(t)
		d.EXPECT().Do(mock.Anything).Return(jsonResp(200, `{not json`), nil)
		if _, err := fixedRefresher(t, d).Refresh(context.Background(), "r"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing access_token", func(t *testing.T) {
		d := mocks.NewHTTPDoer(t)
		d.EXPECT().Do(mock.Anything).Return(jsonResp(200, `{"refresh_token":"x"}`), nil)
		if _, err := fixedRefresher(t, d).Refresh(context.Background(), "r"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("bad base url", func(t *testing.T) {
		r := NewRefresher("http://\x7f", mocks.NewHTTPDoer(t))
		if _, err := r.Refresh(context.Background(), "r"); err == nil {
			t.Fatal("expected error")
		}
	})
}
