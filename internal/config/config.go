// Package config loads and validates cerber's configuration from a YAML file.
// It performs no network access: the config file is the single source of truth
// for which hosts cerber may talk to (see CLAUDE.md, AUDIT.md).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level cerber configuration.
type Config struct {
	Server    Server    `yaml:"server"`
	Access    Access    `yaml:"access"`
	Logging   Logging   `yaml:"logging"`
	TLS       TLS       `yaml:"tls"`
	Usage     Usage     `yaml:"usage"`
	AuthDir   string    `yaml:"auth_dir"` // dir for OAuth tokens written by --claude-login
	Providers Providers `yaml:"providers"`
}

// Usage configures usage persistence and per-model pricing (for cost reporting).
type Usage struct {
	File    string           `yaml:"file"`    // where to persist aggregates (empty = in-memory only)
	Pricing map[string]Price `yaml:"pricing"` // model -> cost per 1M tokens
}

// Price is per-model cost per 1,000,000 tokens.
type Price struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

// Server holds HTTP listener settings.
type Server struct {
	Addr string `yaml:"addr"`
}

// Logging configures the zap logger.
type Logging struct {
	Level string `yaml:"level"` // debug|info|warn|error
	Dir   string `yaml:"dir"`   // log directory; dated files ./logs/<date>.log
}

// TLS configures the HTTPS impersonation listener. DOCKER ONLY: trusting the CA
// and redirecting api.anthropic.com via /etc/hosts on a real machine would
// hijack all Anthropic traffic. With this, Claude Code (no ANTHROPIC_BASE_URL)
// believes it talks to api.anthropic.com and enables 1M context + tool-search.
type TLS struct {
	Enabled bool     `yaml:"enabled"`
	Addr    string   `yaml:"addr"`     // HTTPS listen address (e.g. ":443")
	CertDir string   `yaml:"cert_dir"` // dir holding cert.pem/key.pem/ca.pem
	Hosts   []string `yaml:"hosts"`    // hostnames to impersonate (cert SANs)
	UseDoH  bool     `yaml:"use_doh"`  // resolve upstream via DoH (bypass /etc/hosts)
}

// Access controls who may call cerber. Keys are the API keys clients present.
// AllowLocalhost, when true, accepts any (or no) key from loopback addresses
// (127.0.0.1/::1) — convenient for local single-user setups; remote clients still
// need a valid key.
type Access struct {
	Keys           []string `yaml:"keys"`
	AllowLocalhost bool     `yaml:"allow_localhost"`
	ManagementKey  string   `yaml:"management_key"` // if set, /admin/* requires this key (not client keys)
	KeysFile       string   `yaml:"keys_file"`      // persisted store of dashboard-managed client keys
	// DefaultKeyLimits is applied to newly-created dashboard keys when no explicit
	// limits are given. A zero value means new keys are unlimited. Existing keys
	// keep their own limits (edit via POST /admin/keys/{name}/limits).
	DefaultKeyLimits KeyLimits `yaml:"default_key_limits"`
}

// KeyLimits mirrors access.Limits in config form: a rolling cost budget plus
// rolling request/token rate limits applied per managed client key.
type KeyLimits struct {
	MaxCostUSD   float64 `yaml:"max_cost_usd"`
	BudgetPeriod string  `yaml:"budget_period"` // minute|hour|day|week|month (default month)
	MaxRequests  int64   `yaml:"max_requests"`
	MaxTokens    int64   `yaml:"max_tokens"`
	RatePeriod   string  `yaml:"rate_period"` // minute|hour|day|week|month (default minute)
}

// validKeyPeriod reports whether p is a recognised limit window (empty is valid
// and takes the documented default).
func validKeyPeriod(p string) bool {
	switch p {
	case "", "minute", "hour", "day", "week", "month":
		return true
	default:
		return false
	}
}

// Providers groups upstream provider configuration. Only configured providers
// are reachable; a nil entry means the provider is disabled. Routing maps model
// name prefixes to a provider on the OpenAI-compatible endpoint.
type Providers struct {
	Anthropic *Anthropic `yaml:"anthropic"`
	OpenAI    *OpenAI    `yaml:"openai"`
	Gemini    *Gemini    `yaml:"gemini"`
	Grok      *Grok      `yaml:"grok"`
	Ollama    *Ollama    `yaml:"ollama"`
	Routing   []Route    `yaml:"routing"`
	Strategy  string     `yaml:"strategy"` // credential selection: "round-robin" (default) | "fill-first"
}

// Route maps a model-name prefix to a provider name
// (anthropic|openai|gemini|grok|ollama).
type Route struct {
	Prefix   string `yaml:"prefix"`
	Provider string `yaml:"provider"`
}

// Anthropic configures the Anthropic upstream.
type Anthropic struct {
	BaseURL     string       `yaml:"base_url"`
	Version     string       `yaml:"version"`
	Timeout     Duration     `yaml:"timeout"`
	Credentials []Credential `yaml:"credentials"`
}

// OpenAI configures the OpenAI (OpenAI-compatible) upstream.
type OpenAI struct {
	BaseURL     string       `yaml:"base_url"`
	Timeout     Duration     `yaml:"timeout"`
	Credentials []Credential `yaml:"credentials"`
}

// Gemini configures the Google Generative Language (Gemini) upstream.
type Gemini struct {
	BaseURL     string       `yaml:"base_url"`
	Timeout     Duration     `yaml:"timeout"`
	Credentials []Credential `yaml:"credentials"`
}

// Grok configures the xAI (Grok) upstream, which is OpenAI-compatible.
type Grok struct {
	BaseURL     string       `yaml:"base_url"`
	Timeout     Duration     `yaml:"timeout"`
	Credentials []Credential `yaml:"credentials"`
}

// Ollama configures a local ollama/vLLM upstream, which is OpenAI-compatible.
// These run on the LAN (e.g. a GPU box) and usually need no API key, so
// credentials are optional — main injects a dummy key when none are configured.
type Ollama struct {
	BaseURL       string       `yaml:"base_url"`
	Timeout       Duration     `yaml:"timeout"`
	ProbeInterval Duration     `yaml:"probe_interval"` // liveness + model-discovery poll (0 = default)
	Credentials   []Credential `yaml:"credentials"`
}

// CredentialType enumerates the supported Anthropic auth mechanisms.
type CredentialType string

const (
	// CredentialAPIKey authenticates with an x-api-key header.
	CredentialAPIKey CredentialType = "api_key"
	// CredentialOAuth authenticates with a Claude Code OAuth bearer token.
	CredentialOAuth CredentialType = "oauth"
)

// Credential is a single upstream account credential. Secrets here are only
// ever applied as outbound auth headers to the owning provider; never logged.
type Credential struct {
	Type         CredentialType `yaml:"type"`
	Name         string         `yaml:"name"`
	Key          string         `yaml:"key"`           // api_key
	AccessToken  string         `yaml:"access_token"`  // oauth
	RefreshToken string         `yaml:"refresh_token"` // oauth
	ExpiresAt    time.Time      `yaml:"expires_at"`    // oauth (optional)
}

// Defaults applied when the file omits a value.
const (
	defaultAddr            = ":8080"
	defaultLogLevel        = "info"
	defaultLogDir          = "./logs"
	defaultAuthDir         = "./auths"
	defaultUsageFile       = "./data/usage.json"
	defaultKeysFile        = "./data/keys.json"
	defaultAnthropicBase   = "https://api.anthropic.com"
	defaultAnthropicVer    = "2023-06-01"
	defaultAnthropicWaitNS = 120 * time.Second
	defaultOpenAIBase      = "https://api.openai.com"
	defaultGeminiBase      = "https://generativelanguage.googleapis.com"
	defaultGrokBase        = "https://api.x.ai"
	defaultOllamaBase      = "http://localhost:11434"
	defaultOllamaProbeNS   = 30 * time.Second
	defaultProviderWaitNS  = 120 * time.Second
)

// DefaultAnthropic returns an Anthropic provider config with defaults applied and
// no credentials. Used when the config omits the anthropic block but OAuth tokens
// are present on disk (auth_dir).
func DefaultAnthropic() *Anthropic {
	return &Anthropic{
		BaseURL: defaultAnthropicBase,
		Version: defaultAnthropicVer,
		Timeout: Duration(defaultAnthropicWaitNS),
	}
}

// DefaultGrok returns a Grok provider config with defaults and no credentials.
// Used when the config omits the grok block but xAI OAuth tokens are present on
// disk (auth_dir/xai).
func DefaultGrok() *Grok {
	return &Grok{BaseURL: defaultGrokBase, Timeout: Duration(defaultProviderWaitNS)}
}

// Load reads, parses, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse decodes config from raw YAML bytes, applying defaults and validating.
// ${VAR} / $VAR references in the YAML are expanded from the process environment
// (load a .env first with LoadEnvFile) so secrets can live outside the file.
func Parse(data []byte) (*Config, error) {
	expanded := os.Expand(string(data), os.Getenv)
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = defaultAddr
	}
	if c.Logging.Level == "" {
		c.Logging.Level = defaultLogLevel
	}
	if c.Logging.Dir == "" {
		c.Logging.Dir = defaultLogDir
	}
	if c.AuthDir == "" {
		c.AuthDir = defaultAuthDir
	}
	if c.Usage.File == "" {
		c.Usage.File = defaultUsageFile
	}
	if c.Access.KeysFile == "" {
		c.Access.KeysFile = defaultKeysFile
	}
	if c.TLS.Enabled {
		if c.TLS.Addr == "" {
			c.TLS.Addr = ":443"
		}
		if c.TLS.CertDir == "" {
			c.TLS.CertDir = "./certs"
		}
		if len(c.TLS.Hosts) == 0 {
			c.TLS.Hosts = []string{"api.anthropic.com"}
		}
	}
	if a := c.Providers.Anthropic; a != nil {
		if a.BaseURL == "" {
			a.BaseURL = defaultAnthropicBase
		}
		if a.Version == "" {
			a.Version = defaultAnthropicVer
		}
		if a.Timeout == 0 {
			a.Timeout = Duration(defaultAnthropicWaitNS)
		}
	}
	if o := c.Providers.OpenAI; o != nil {
		if o.BaseURL == "" {
			o.BaseURL = defaultOpenAIBase
		}
		if o.Timeout == 0 {
			o.Timeout = Duration(defaultProviderWaitNS)
		}
	}
	if g := c.Providers.Gemini; g != nil {
		if g.BaseURL == "" {
			g.BaseURL = defaultGeminiBase
		}
		if g.Timeout == 0 {
			g.Timeout = Duration(defaultProviderWaitNS)
		}
	}
	if g := c.Providers.Grok; g != nil {
		if g.BaseURL == "" {
			g.BaseURL = defaultGrokBase
		}
		if g.Timeout == 0 {
			g.Timeout = Duration(defaultProviderWaitNS)
		}
	}
	if o := c.Providers.Ollama; o != nil {
		if o.BaseURL == "" {
			o.BaseURL = defaultOllamaBase
		}
		if o.Timeout == 0 {
			o.Timeout = Duration(defaultProviderWaitNS)
		}
		if o.ProbeInterval == 0 {
			o.ProbeInterval = Duration(defaultOllamaProbeNS)
		}
	}
}

// Validate reports the first configuration error found.
func (c *Config) Validate() error {
	if len(c.Access.Keys) == 0 && !c.Access.AllowLocalhost {
		return fmt.Errorf("config: access.keys must list at least one client key (or set access.allow_localhost: true)")
	}
	for i, k := range c.Access.Keys {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("config: access.keys[%d] is empty", i)
		}
	}
	if l := c.Access.DefaultKeyLimits; !validKeyPeriod(l.BudgetPeriod) || !validKeyPeriod(l.RatePeriod) {
		return fmt.Errorf("config: access.default_key_limits period must be minute|hour|day|week|month")
	}
	p := c.Providers
	if p.Anthropic == nil && p.OpenAI == nil && p.Gemini == nil && p.Grok == nil && p.Ollama == nil {
		return fmt.Errorf("config: no providers configured")
	}
	if p.Anthropic != nil {
		// Anthropic credentials may be empty here: --claude-login writes OAuth
		// tokens to auth_dir which are merged in at startup. main enforces a
		// non-empty merged set.
		if err := validateCreds("anthropic", p.Anthropic.BaseURL, p.Anthropic.Credentials, false); err != nil {
			return err
		}
	}
	if p.OpenAI != nil {
		if err := validateCreds("openai", p.OpenAI.BaseURL, p.OpenAI.Credentials, true); err != nil {
			return err
		}
	}
	if p.Gemini != nil {
		if err := validateCreds("gemini", p.Gemini.BaseURL, p.Gemini.Credentials, true); err != nil {
			return err
		}
	}
	if p.Grok != nil {
		if err := validateCreds("grok", p.Grok.BaseURL, p.Grok.Credentials, true); err != nil {
			return err
		}
	}
	if p.Ollama != nil {
		// Local ollama/vLLM needs no key: credentials are optional.
		if err := validateCreds("ollama", p.Ollama.BaseURL, p.Ollama.Credentials, false); err != nil {
			return err
		}
	}
	switch p.Strategy {
	case "", "round-robin", "fill-first":
	default:
		return fmt.Errorf("config: providers.strategy %q is not round-robin|fill-first", p.Strategy)
	}
	for i, r := range p.Routing {
		switch r.Provider {
		case "anthropic", "openai", "gemini", "grok", "ollama":
		default:
			return fmt.Errorf("config: providers.routing[%d].provider %q is not anthropic|openai|gemini|grok|ollama", i, r.Provider)
		}
		if strings.TrimSpace(r.Prefix) == "" {
			return fmt.Errorf("config: providers.routing[%d].prefix is empty", i)
		}
	}
	return nil
}

// validateCreds checks a provider's base URL and credentials. If requireCred is
// false, an empty credential list is allowed (credentials may come from disk).
func validateCreds(name, baseURL string, creds []Credential, requireCred bool) error {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: providers.%s.base_url must be an http(s) URL, got %q", name, baseURL)
	}
	if requireCred && len(creds) == 0 {
		return fmt.Errorf("config: providers.%s.credentials must list at least one credential", name)
	}
	for i := range creds {
		if err := creds[i].validate(); err != nil {
			return fmt.Errorf("config: providers.%s.credentials[%d]: %w", name, i, err)
		}
	}
	return nil
}

func (c *Credential) validate() error {
	switch c.Type {
	case CredentialAPIKey:
		if strings.TrimSpace(c.Key) == "" {
			return fmt.Errorf("api_key credential requires a non-empty key")
		}
	case CredentialOAuth:
		if strings.TrimSpace(c.AccessToken) == "" {
			return fmt.Errorf("oauth credential requires a non-empty access_token")
		}
	case "":
		return fmt.Errorf("missing type (want %q or %q)", CredentialAPIKey, CredentialOAuth)
	default:
		return fmt.Errorf("unknown type %q", c.Type)
	}
	return nil
}

// LoadEnvFile loads KEY=VALUE pairs from a .env file into the process
// environment. Existing environment variables are not overwritten (real env
// wins). A missing file is not an error. Blank lines and # comments are ignored;
// surrounding single or double quotes around a value are stripped.
func LoadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read env file: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, val); err != nil {
				return fmt.Errorf("set env %q: %w", key, err)
			}
		}
	}
	return nil
}

// Duration is a time.Duration that unmarshals from a YAML string like "120s".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"120s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }
