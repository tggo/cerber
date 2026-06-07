// Package config loads and validates cerber's configuration from a YAML file.
// It performs no network access: the config file is the single source of truth
// for which hosts cerber may talk to (see CLAUDE.md, AUDIT.md).
package config

import (
	"fmt"
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
	Providers Providers `yaml:"providers"`
}

// Server holds HTTP listener settings.
type Server struct {
	Addr string `yaml:"addr"`
}

// Access controls who may call cerber. Keys are the API keys clients present.
type Access struct {
	Keys []string `yaml:"keys"`
}

// Providers groups upstream provider configuration. Only configured providers
// are reachable; a nil entry means the provider is disabled.
type Providers struct {
	Anthropic *Anthropic `yaml:"anthropic"`
}

// Anthropic configures the Anthropic upstream.
type Anthropic struct {
	BaseURL     string       `yaml:"base_url"`
	Version     string       `yaml:"version"`
	Timeout     Duration     `yaml:"timeout"`
	Credentials []Credential `yaml:"credentials"`
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
	defaultAnthropicBase   = "https://api.anthropic.com"
	defaultAnthropicVer    = "2023-06-01"
	defaultAnthropicWaitNS = 120 * time.Second
)

// Load reads, parses, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse decodes config from raw YAML bytes, applying defaults and validating.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
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
}

// Validate reports the first configuration error found.
func (c *Config) Validate() error {
	if len(c.Access.Keys) == 0 {
		return fmt.Errorf("config: access.keys must list at least one client key")
	}
	for i, k := range c.Access.Keys {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("config: access.keys[%d] is empty", i)
		}
	}
	if c.Providers.Anthropic == nil {
		return fmt.Errorf("config: no providers configured")
	}
	return c.Providers.Anthropic.validate()
}

func (a *Anthropic) validate() error {
	u, err := url.Parse(a.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: providers.anthropic.base_url must be an http(s) URL, got %q", a.BaseURL)
	}
	if len(a.Credentials) == 0 {
		return fmt.Errorf("config: providers.anthropic.credentials must list at least one credential")
	}
	for i := range a.Credentials {
		if err := a.Credentials[i].validate(); err != nil {
			return fmt.Errorf("config: providers.anthropic.credentials[%d]: %w", i, err)
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
