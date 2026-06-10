package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
)

func TestModelAlias_RewritesUpstreamBody(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	s.SetModelAliases(map[string]string{"opus": "claude-opus-4-x"})

	// The body forwarded to Anthropic must carry the canonical model, not "opus".
	up.EXPECT().Send(mock.Anything,
		mock.MatchedBy(func(b []byte) bool {
			return strings.Contains(string(b), "claude-opus-4-x") && !strings.Contains(string(b), `"opus"`)
		}),
		false, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", `{"id":"m"}`), nil).Once()

	rec := do(t, s.Handler(), "POST", "/v1/messages", `{"model":"opus","stream":false}`, clientKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("aliased request = %d, want 200", rec.Code)
	}
}

func TestModelAlias_UntouchedWhenNoAlias(t *testing.T) {
	// No alias configured for "claude-real": body must be forwarded unchanged.
	s, up := newServer(t, newStore(t, 1))
	s.SetModelAliases(map[string]string{"opus": "claude-opus-4-x"})
	up.EXPECT().Send(mock.Anything,
		mock.MatchedBy(func(b []byte) bool { return strings.Contains(string(b), `"claude-real"`) }),
		false, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", `{"id":"m"}`), nil).Once()
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{"model":"claude-real","stream":false}`, clientKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("request = %d, want 200", rec.Code)
	}
}

func TestSetModelField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool // ok
	}{
		{"object", `{"model":"a","x":1}`, true},
		{"no model key", `{"x":1}`, true},
		{"not an object", `["a","b"]`, false},
		{"garbage", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := setModelField([]byte(tc.in), "Z")
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if ok && !strings.Contains(string(out), `"model":"Z"`) {
				t.Errorf("model not set: %s", out)
			}
			if !ok && string(out) != tc.in {
				t.Errorf("non-object body mutated: %s", out)
			}
		})
	}
}
