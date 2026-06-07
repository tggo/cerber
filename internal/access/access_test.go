package access

import (
	"net/http/httptest"
	"testing"
)

func TestAllow(t *testing.T) {
	c := New([]string{"key-one", "key-two"})
	cases := map[string]bool{
		"key-one": true,
		"key-two": true,
		"key-thr": false,
		"":        false,
		"KEY-ONE": false, // case-sensitive
		"key-on":  false, // length differs
	}
	for in, want := range cases {
		if got := c.Allow(in); got != want {
			t.Errorf("Allow(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNew_CopiesKeys(t *testing.T) {
	src := []string{"k"}
	c := New(src)
	src[0] = "mutated"
	if !c.Allow("k") {
		t.Error("Checker should not be affected by caller mutating the slice")
	}
}

func TestFromRequest(t *testing.T) {
	cases := []struct {
		name   string
		auth   string
		apiKey string
		want   string
	}{
		{"bearer", "Bearer abc", "", "abc"},
		{"bearer trimmed", "Bearer  abc ", "", "abc"},
		{"x-api-key", "", "xyz", "xyz"},
		{"bearer wins", "Bearer abc", "xyz", "abc"},
		{"non-bearer auth falls back", "Basic zzz", "xyz", "xyz"},
		{"none", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			if tc.apiKey != "" {
				r.Header.Set("x-api-key", tc.apiKey)
			}
			if got := FromRequest(r); got != tc.want {
				t.Errorf("FromRequest = %q, want %q", got, tc.want)
			}
		})
	}
}
