package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const validYAML = `
server:
  addr: ":9000"
access:
  keys:
    - "client-key-1"
providers:
  anthropic:
    credentials:
      - type: api_key
        name: acct-a
        key: "sk-ant-xxx"
`

func TestParse_ValidWithDefaults(t *testing.T) {
	// base_url, version, timeout omitted -> defaults applied.
	c, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Server.Addr != ":9000" {
		t.Errorf("addr = %q", c.Server.Addr)
	}
	a := c.Providers.Anthropic
	if a.BaseURL != defaultAnthropicBase {
		t.Errorf("base_url = %q, want default", a.BaseURL)
	}
	if a.Version != defaultAnthropicVer {
		t.Errorf("version = %q, want default", a.Version)
	}
	if a.Timeout.Std() != defaultAnthropicWaitNS {
		t.Errorf("timeout = %v, want default", a.Timeout.Std())
	}
}

func TestParse_DefaultAddr(t *testing.T) {
	y := `
access: {keys: ["k"]}
providers:
  anthropic:
    credentials:
      - {type: oauth, access_token: "tok"}
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Server.Addr != defaultAddr {
		t.Errorf("addr = %q, want %q", c.Server.Addr, defaultAddr)
	}
}

func TestParse_CustomTimeoutAndURL(t *testing.T) {
	y := `
access: {keys: ["k"]}
providers:
  anthropic:
    base_url: "http://localhost:1234"
    version: "2024-01-01"
    timeout: "30s"
    credentials:
      - {type: api_key, key: "k"}
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := c.Providers.Anthropic
	if a.Timeout.Std() != 30*time.Second {
		t.Errorf("timeout = %v", a.Timeout.Std())
	}
	if a.BaseURL != "http://localhost:1234" || a.Version != "2024-01-01" {
		t.Errorf("base/version not preserved: %q %q", a.BaseURL, a.Version)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := map[string]string{
		"bad yaml":          `:::not yaml`,
		"unknown field":     "access: {keys: [k]}\nproviders: {anthropic: {credentials: [{type: api_key, key: k}]}}\nbogus: 1",
		"no access keys":    "access: {keys: []}\nproviders: {anthropic: {credentials: [{type: api_key, key: k}]}}",
		"empty access key":  "access: {keys: [\"  \"]}\nproviders: {anthropic: {credentials: [{type: api_key, key: k}]}}",
		"no providers":      "access: {keys: [k]}",
		"bad base_url":      "access: {keys: [k]}\nproviders: {anthropic: {base_url: \"ftp://x\", credentials: [{type: api_key, key: k}]}}",
		"no creds":          "access: {keys: [k]}\nproviders: {anthropic: {credentials: []}}",
		"apikey no key":     "access: {keys: [k]}\nproviders: {anthropic: {credentials: [{type: api_key}]}}",
		"oauth no token":    "access: {keys: [k]}\nproviders: {anthropic: {credentials: [{type: oauth}]}}",
		"missing cred type": "access: {keys: [k]}\nproviders: {anthropic: {credentials: [{key: k}]}}",
		"unknown cred type": "access: {keys: [k]}\nproviders: {anthropic: {credentials: [{type: magic, key: k}]}}",
		"bad duration":      "access: {keys: [k]}\nproviders: {anthropic: {timeout: \"notaduration\", credentials: [{type: api_key, key: k}]}}",
		"duration not str":  "access: {keys: [k]}\nproviders: {anthropic: {timeout: 5, credentials: [{type: api_key, key: k}]}}",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(y)); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestParse_LoggingDefaults(t *testing.T) {
	c, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Logging.Level != defaultLogLevel || c.Logging.Dir != defaultLogDir {
		t.Errorf("logging defaults = %+v", c.Logging)
	}
}

func TestParse_EnvSubstitution(t *testing.T) {
	t.Setenv("CERBER_TEST_KEY", "sk-from-env")
	y := `
access: {keys: ["k"]}
providers:
  anthropic:
    credentials:
      - {type: api_key, key: "${CERBER_TEST_KEY}"}
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Providers.Anthropic.Credentials[0].Key != "sk-from-env" {
		t.Errorf("env not substituted: %q", c.Providers.Anthropic.Credentials[0].Key)
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	content := "# comment\n\nexport FOO=bar\nQUOTED=\"q v\"\nSINGLE='s'\nPRESET=fromfile\nnoequals\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRESET", "fromenv") // must not be overwritten
	if err := LoadEnvFile(p); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	checks := map[string]string{"FOO": "bar", "QUOTED": "q v", "SINGLE": "s", "PRESET": "fromenv"}
	for k, want := range checks {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// Missing file is not an error.
	if err := LoadEnvFile(filepath.Join(dir, "nope.env")); err != nil {
		t.Errorf("missing .env should be nil, got %v", err)
	}
}

func TestLoad_FileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
