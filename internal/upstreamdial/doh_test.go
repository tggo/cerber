package upstreamdial

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type stubDoer struct {
	body   string
	status int
	calls  int
}

func (s *stubDoer) Do(*http.Request) (*http.Response, error) {
	s.calls++
	st := s.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(s.body)), Header: http.Header{}}, nil
}

func TestResolve_AndCache(t *testing.T) {
	now := time.Unix(1000, 0)
	d := &stubDoer{body: `{"Answer":[{"type":5,"data":"cname.x"},{"type":1,"TTL":300,"data":"1.2.3.4"}]}`}
	r := NewResolver(WithDoer(d), WithClock(func() time.Time { return now }), WithEndpoint("https://1.1.1.1/dns-query"))

	ip, err := r.Resolve(context.Background(), "api.anthropic.com")
	if err != nil || ip != "1.2.3.4" {
		t.Fatalf("resolve = %q %v", ip, err)
	}
	// second call is cached (no new request)
	if _, err := r.Resolve(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
	if d.calls != 1 {
		t.Errorf("calls = %d, want 1 (cached)", d.calls)
	}
	// after TTL, re-queries
	now = now.Add(301 * time.Second)
	if _, err := r.Resolve(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
	if d.calls != 2 {
		t.Errorf("calls = %d, want 2 (ttl expired)", d.calls)
	}
}

func TestResolve_Errors(t *testing.T) {
	now := func() time.Time { return time.Unix(0, 0) }
	t.Run("no A record", func(t *testing.T) {
		r := NewResolver(WithDoer(&stubDoer{body: `{"Answer":[{"type":5,"data":"x"}]}`}), WithClock(now))
		if _, err := r.Resolve(context.Background(), "h"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("non-200", func(t *testing.T) {
		r := NewResolver(WithDoer(&stubDoer{status: 500, body: ``}), WithClock(now))
		if _, err := r.Resolve(context.Background(), "h"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("bad json", func(t *testing.T) {
		r := NewResolver(WithDoer(&stubDoer{body: `{`}), WithClock(now))
		if _, err := r.Resolve(context.Background(), "h"); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestDialContext_IPLiteralSkipsResolve(t *testing.T) {
	d := &stubDoer{body: `{}`}
	r := NewResolver(WithDoer(d))
	// Dialing an IP literal should not call DoH (it'll try to connect and fail, that's fine).
	_, _ = r.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if d.calls != 0 {
		t.Errorf("DoH called for IP literal: %d", d.calls)
	}
}
