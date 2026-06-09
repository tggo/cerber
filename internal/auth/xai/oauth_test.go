package xai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tggo/cerber/internal/provider/mocks"

	"github.com/stretchr/testify/mock"
)

func resp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func TestStartDevice(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var gotURL, gotBody string
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return resp(200, `{"device_code":"dev123","user_code":"ABCD-EFGH","verification_uri":"https://accounts.x.ai/oauth2/device","verification_uri_complete":"https://accounts.x.ai/oauth2/device?code=ABCD-EFGH","interval":5,"expires_in":900}`), nil
	})
	dc, err := StartDevice(context.Background(), doer)
	if err != nil {
		t.Fatalf("StartDevice: %v", err)
	}
	if gotURL != DeviceCodeURL {
		t.Errorf("url = %s", gotURL)
	}
	if !strings.Contains(gotBody, "client_id="+ClientID) || !strings.Contains(gotBody, "scope=") {
		t.Errorf("body = %s", gotBody)
	}
	if dc.DeviceCode != "dev123" || dc.UserCode != "ABCD-EFGH" || dc.Interval != 5 {
		t.Errorf("dc = %+v", dc)
	}
}

func TestPollToken_PendingSlowDownSuccess(t *testing.T) {
	now := func() time.Time { return time.Unix(1000, 0) }

	d1 := mocks.NewHTTPDoer(t)
	d1.EXPECT().Do(mock.Anything).Return(resp(400, `{"error":"authorization_pending"}`), nil)
	if _, err := PollToken(context.Background(), d1, "dev", now); !errors.Is(err, ErrAuthorizationPending) {
		t.Errorf("pending = %v", err)
	}

	d2 := mocks.NewHTTPDoer(t)
	d2.EXPECT().Do(mock.Anything).Return(resp(400, `{"error":"slow_down"}`), nil)
	if _, err := PollToken(context.Background(), d2, "dev", now); !errors.Is(err, ErrSlowDown) {
		t.Errorf("slow_down = %v", err)
	}

	d3 := mocks.NewHTTPDoer(t)
	var body string
	d3.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		return resp(200, `{"access_token":"at","refresh_token":"rt","expires_in":3600,"token_type":"Bearer"}`), nil
	})
	tok, err := PollToken(context.Background(), d3, "dev", now)
	if err != nil {
		t.Fatalf("success: %v", err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" || !tok.ExpiresAt.Equal(time.Unix(4600, 0)) {
		t.Errorf("tok = %+v", tok)
	}
	if !strings.Contains(body, "grant_type=urn") || !strings.Contains(body, "device_code=dev") {
		t.Errorf("poll body = %s", body)
	}
}

func TestPollToken_HardError(t *testing.T) {
	d := mocks.NewHTTPDoer(t)
	d.EXPECT().Do(mock.Anything).Return(resp(400, `{"error":"access_denied","error_description":"user declined"}`), nil)
	_, err := PollToken(context.Background(), d, "dev", nil)
	if err == nil || errors.Is(err, ErrAuthorizationPending) || errors.Is(err, ErrSlowDown) {
		t.Errorf("access_denied = %v, want hard error", err)
	}
	if !strings.Contains(err.Error(), "user declined") {
		t.Errorf("err = %v", err)
	}
}

func TestRefresh(t *testing.T) {
	d := mocks.NewHTTPDoer(t)
	var body string
	d.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		return resp(200, `{"access_token":"at2","refresh_token":"rt2","expires_in":3600}`), nil
	})
	tok, err := Refresh(context.Background(), d, "old-rt", func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.AccessToken != "at2" || tok.RefreshToken != "rt2" {
		t.Errorf("tok = %+v", tok)
	}
	if !strings.Contains(body, "grant_type=refresh_token") || !strings.Contains(body, "refresh_token=old-rt") {
		t.Errorf("refresh body = %s", body)
	}
}
