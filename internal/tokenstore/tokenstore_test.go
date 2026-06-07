package tokenstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"cerber/internal/config"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auths")
	exp := time.Unix(5000, 0).UTC()
	path, err := Save(dir, "u@e.com", Record{
		Name: "u@e.com", AccessToken: "acc", RefreshToken: "ref", Email: "u@e.com", ExpiresAt: exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "u_e.com.json" {
		t.Errorf("sanitized path = %s", path)
	}
	// file is 0600
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v", info.Mode().Perm())
	}

	creds, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("creds = %d", len(creds))
	}
	c := creds[0]
	if c.Type != config.CredentialOAuth || c.Name != "u@e.com" || c.AccessToken != "acc" ||
		c.RefreshToken != "ref" || !c.ExpiresAt.Equal(exp) {
		t.Errorf("loaded = %+v", c)
	}
}

func TestLoad_MissingDir(t *testing.T) {
	creds, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil || creds != nil {
		t.Errorf("missing dir = %v %v", creds, err)
	}
}

func TestLoad_SkipsAndErrors(t *testing.T) {
	dir := t.TempDir()
	// non-json ignored
	os.WriteFile(filepath.Join(dir, "note.txt"), []byte("x"), 0o600)
	// empty-access-token record skipped
	os.WriteFile(filepath.Join(dir, "empty.json"), []byte(`{"type":"oauth","refresh_token":"r"}`), 0o600)
	// valid, name falls back to filename
	os.WriteFile(filepath.Join(dir, "acct.json"), []byte(`{"type":"oauth","access_token":"a"}`), 0o600)

	creds, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].Name != "acct" || creds[0].AccessToken != "a" {
		t.Fatalf("creds = %+v", creds)
	}

	// malformed json -> error
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{`), 0o600)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{"a@b.com": "a_b.com", "ok-name_1": "ok-name_1", "": "claude", "a/b c": "a_b_c"}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
