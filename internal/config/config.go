// Package config stores this device's identity and preferences in the user's
// config directory (e.g. ~/Library/Application Support/bitflow/config.json).
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Config is persisted as JSON. Zero values mean "use the default".
type Config struct {
	ID       string `json:"id"`             // stable random device ID (discovery dedupe)
	Name     string `json:"name,omitempty"` // shown to other devices; default: hostname
	Dest     string `json:"dest,omitempty"` // default receive folder; default: ~/Downloads/bit-flow
	Conflict string `json:"on_conflict,omitempty"`

	path string
}

// Dir is the bit-flow config directory.
func Dir() (string, error) {
	if d := os.Getenv("BITFLOW_CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bitflow"), nil
}

// Load reads the config, creating a device ID on first use.
func Load() (*Config, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	c := &Config{path: filepath.Join(dir, "config.json")}
	b, err := os.ReadFile(c.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, c); err != nil {
			return nil, err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if c.ID == "" {
		var id [8]byte
		_, _ = rand.Read(id[:])
		c.ID = hex.EncodeToString(id[:])
		if err := c.Save(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Save writes the config atomically.
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// DeviceName is the configured name or a cleaned-up hostname.
func (c *Config) DeviceName() string {
	if c.Name != "" {
		return c.Name
	}
	h, _ := os.Hostname()
	h = strings.TrimSuffix(h, ".local")
	h = strings.TrimSuffix(h, ".lan")
	if h == "" {
		return "bit-flow device"
	}
	return h
}

// ReceiveDir is the configured default folder for incoming files.
func (c *Config) ReceiveDir() string {
	if c.Dest != "" {
		return c.Dest
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads", "bit-flow")
}
