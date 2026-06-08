package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/credential"
)

func store(t *testing.T, n int) *credential.Store {
	t.Helper()
	var cfgs []config.Credential
	for i := 0; i < n; i++ {
		cfgs = append(cfgs, config.Credential{Type: config.CredentialAPIKey, Name: string(rune('a' + i)), Key: "k"})
	}
	s, err := credential.NewStore(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func resp(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("body"))}
}

func TestRotate_Success(t *testing.T) {
	r, cred, err := Rotate(context.Background(), store(t, 2), time.Minute, func(c *credential.Credential) (*http.Response, error) {
		return resp(200), nil
	})
	if err != nil || cred != "a" || r.StatusCode != 200 {
		t.Fatalf("got %v %q %v", err, cred, r)
	}
	r.Body.Close()
}

func TestRotate_On429ThenSuccess(t *testing.T) {
	n := 0
	r, cred, err := Rotate(context.Background(), store(t, 2), time.Minute, func(c *credential.Credential) (*http.Response, error) {
		n++
		if n == 1 {
			return resp(429), nil
		}
		return resp(200), nil
	})
	if err != nil || cred != "b" || n != 2 {
		t.Fatalf("got %v %q n=%d", err, cred, n)
	}
	r.Body.Close()
}

func TestRotate_AllFail(t *testing.T) {
	_, _, err := Rotate(context.Background(), store(t, 2), time.Minute, func(c *credential.Credential) (*http.Response, error) {
		return resp(401), nil
	})
	if err == nil {
		t.Fatal("expected error when all creds fail")
	}
}

func TestRotate_TransportError(t *testing.T) {
	_, _, err := Rotate(context.Background(), store(t, 1), time.Minute, func(c *credential.Credential) (*http.Response, error) {
		return nil, errors.New("dial")
	})
	if err == nil {
		t.Fatal("expected transport error")
	}
}

func TestRotate_NoneAvailable(t *testing.T) {
	s := store(t, 1)
	c, _ := s.Next()
	s.Cooldown(c, time.Hour)
	_, _, err := Rotate(context.Background(), s, time.Minute, func(*credential.Credential) (*http.Response, error) {
		t.Fatal("send should not be called")
		return nil, nil
	})
	if !errors.Is(err, credential.ErrNoneAvailable) {
		t.Fatalf("err = %v, want ErrNoneAvailable", err)
	}
}

func TestRotate_ClientCancelNoCooldown(t *testing.T) {
	s := store(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := Rotate(ctx, s, time.Minute, func(*credential.Credential) (*http.Response, error) {
		return nil, context.Canceled
	})
	if err == nil {
		t.Fatal("expected error")
	}
	// credential must NOT be cooled down after a client cancellation
	if _, e := s.Next(); e != nil {
		t.Errorf("credential should still be available after client cancel, got %v", e)
	}
}
