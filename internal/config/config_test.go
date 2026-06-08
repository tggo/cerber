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

func TestParse_OpenAIAndGeminiDefaults(t *testing.T) {
	y := `
access: {keys: ["k"]}
providers:
  openai:
    credentials: [{type: api_key, key: "sk-o"}]
  gemini:
    credentials: [{type: api_key, key: "g"}]
  routing:
    - {prefix: "gpt", provider: openai}
    - {prefix: "gemini", provider: gemini}
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Providers.Anthropic != nil {
		t.Error("anthropic should be nil")
	}
	if c.Providers.OpenAI.BaseURL != defaultOpenAIBase || c.Providers.OpenAI.Timeout.Std() != defaultProviderWaitNS {
		t.Errorf("openai defaults = %+v", c.Providers.OpenAI)
	}
	if c.Providers.Gemini.BaseURL != defaultGeminiBase {
		t.Errorf("gemini base = %q", c.Providers.Gemini.BaseURL)
	}
	if c.Providers.Grok != nil {
		t.Error("grok should be nil when omitted")
	}
	if len(c.Providers.Routing) != 2 {
		t.Errorf("routing = %+v", c.Providers.Routing)
	}
}

func TestParse_GrokDefaults(t *testing.T) {
	y := "access: {keys: [k]}\nproviders: {grok: {credentials: [{type: api_key, key: x}]}, routing: [{prefix: grok, provider: grok}]}"
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Providers.Grok.BaseURL != defaultGrokBase || c.Providers.Grok.Timeout.Std() != defaultProviderWaitNS {
		t.Errorf("grok defaults = %+v", c.Providers.Grok)
	}
}

func TestParse_OllamaDefaultsNoCreds(t *testing.T) {
	// Local ollama needs no key: an empty credential list is valid, and it can
	// be the only configured provider.
	y := "access: {keys: [k]}\nproviders: {ollama: {}}"
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Providers.Ollama == nil {
		t.Fatal("ollama should be set")
	}
	if c.Providers.Ollama.BaseURL != defaultOllamaBase || c.Providers.Ollama.Timeout.Std() != defaultProviderWaitNS {
		t.Errorf("ollama defaults = %+v", c.Providers.Ollama)
	}
}

func TestParse_OllamaRouting(t *testing.T) {
	y := "access: {keys: [k]}\nproviders: {ollama: {base_url: \"http://gpu0:11434\"}, routing: [{prefix: llama, provider: ollama}]}"
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Providers.Ollama.BaseURL != "http://gpu0:11434" {
		t.Errorf("ollama base = %q", c.Providers.Ollama.BaseURL)
	}
}

func TestParse_ProviderErrors(t *testing.T) {
	cases := map[string]string{
		"openai bad url":    "access: {keys: [k]}\nproviders: {openai: {base_url: \"ftp://x\", credentials: [{type: api_key, key: k}]}}",
		"openai no creds":   "access: {keys: [k]}\nproviders: {openai: {credentials: []}}",
		"openai bad cred":   "access: {keys: [k]}\nproviders: {openai: {credentials: [{type: api_key}]}}",
		"gemini bad url":    "access: {keys: [k]}\nproviders: {gemini: {base_url: \"::\", credentials: [{type: api_key, key: k}]}}",
		"gemini no creds":   "access: {keys: [k]}\nproviders: {gemini: {credentials: []}}",
		"route bad prov":    "access: {keys: [k]}\nproviders: {openai: {credentials: [{type: api_key, key: k}]}, routing: [{prefix: gpt, provider: bogus}]}",
		"route no prefix":   "access: {keys: [k]}\nproviders: {openai: {credentials: [{type: api_key, key: k}]}, routing: [{prefix: \"\", provider: openai}]}",
		"truly no provider": "access: {keys: [k]}\nproviders: {}",
		"grok no creds":     "access: {keys: [k]}\nproviders: {grok: {credentials: []}}",
		"ollama bad url":    "access: {keys: [k]}\nproviders: {ollama: {base_url: \"ftp://x\"}}",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(y)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestDefaultAnthropic(t *testing.T) {
	a := DefaultAnthropic()
	if a.BaseURL != defaultAnthropicBase || a.Version != defaultAnthropicVer || a.Timeout.Std() != defaultAnthropicWaitNS {
		t.Errorf("DefaultAnthropic = %+v", a)
	}
	if len(a.Credentials) != 0 {
		t.Errorf("expected no credentials, got %d", len(a.Credentials))
	}
}

func TestParse_NilAnthropicWithOtherProvider(t *testing.T) {
	// anthropic omitted, openai present -> valid (anthropic may be filled from auth_dir at runtime)
	y := "access: {keys: [k]}\nproviders: {openai: {credentials: [{type: api_key, key: k}]}}"
	if _, err := Parse([]byte(y)); err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
}

func TestParse_TLSDefaults(t *testing.T) {
	y := "access: {keys: [k]}\ntls: {enabled: true, use_doh: true}\nproviders: {anthropic: {credentials: [{type: api_key, key: k}]}}"
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.TLS.Enabled || c.TLS.Addr != ":443" || c.TLS.CertDir != "./certs" ||
		len(c.TLS.Hosts) != 1 || c.TLS.Hosts[0] != "api.anthropic.com" || !c.TLS.UseDoH {
		t.Errorf("tls defaults = %+v", c.TLS)
	}
	// disabled -> no defaults applied
	c2, _ := Parse([]byte("access: {keys: [k]}\nproviders: {anthropic: {credentials: [{type: api_key, key: k}]}}"))
	if c2.TLS.Enabled || c2.TLS.Addr != "" {
		t.Errorf("tls should stay zero when disabled: %+v", c2.TLS)
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
