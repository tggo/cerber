// Package tokenstore persists OAuth credentials to disk so they survive restarts
// and can be loaded alongside config credentials. Files are written 0600 in a
// directory created 0700. This is the only place cerber writes secrets to disk,
// and it is never logged (see CLAUDE.md).
package tokenstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tggo/cerber/internal/config"
)

// Record is one persisted OAuth credential.
type Record struct {
	Type         string    `json:"type"` // always "oauth"
	Name         string    `json:"name"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Email        string    `json:"email,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Save writes r as <dir>/<name>.json (0600), creating dir (0700) if needed, and
// returns the file path. The name is sanitized for filesystem safety.
func Save(dir, name string, r Record) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("tokenstore: create dir: %w", err)
	}
	r.Type = "oauth"
	if r.Name == "" {
		r.Name = name
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("tokenstore: marshal: %w", err)
	}
	path := filepath.Join(dir, sanitize(name)+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("tokenstore: write: %w", err)
	}
	return path, nil
}

// Load reads every *.json OAuth record in dir into config credentials. A missing
// directory yields no credentials and no error.
func Load(dir string) ([]config.Credential, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("tokenstore: read dir: %w", err)
	}
	var creds []config.Credential
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("tokenstore: read %s: %w", e.Name(), err)
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("tokenstore: parse %s: %w", e.Name(), err)
		}
		if r.AccessToken == "" {
			continue
		}
		name := r.Name
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".json")
		}
		creds = append(creds, config.Credential{
			Type:         config.CredentialOAuth,
			Name:         name,
			AccessToken:  r.AccessToken,
			RefreshToken: r.RefreshToken,
			ExpiresAt:    r.ExpiresAt,
		})
	}
	return creds, nil
}

// sanitize keeps only filesystem-safe characters in a credential name.
func sanitize(name string) string {
	if name == "" {
		return "claude"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
