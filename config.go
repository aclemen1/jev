// Package jev asks TypeSafe's System One API (/v1/systemone): the hosted Jev
// first, a local OpenJev when Jev refuses or does not answer. A refusal for
// credit or key puts the provider in cooldown and sends one alert per
// outage and kind; a success of the provider clears its cooldown and ends
// the outage.
package jev

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is ~/.config/jev/config.yaml.
type Config struct {
	// Providers are tried in order: the first that answers wins.
	Providers []Provider `yaml:"providers"`
	// Cooldown is how long a provider refused for credit or key is skipped; default 1h.
	Cooldown Duration `yaml:"cooldown,omitempty"`
	Alert    Alert    `yaml:"alert,omitempty"`
	// StateDir holds state.json, alerts.json and the pools; default ~/.local/state/jev ($JEV_STATE_DIR).
	StateDir string `yaml:"state_dir,omitempty"`

	path string
}

// Provider is one /v1/systemone server.
type Provider struct {
	Name  string `yaml:"name"`
	URL   string `yaml:"url"` // full endpoint, e.g. http://127.0.0.1:3000/v1/systemone
	Model string `yaml:"model,omitempty"`
	Key   Key    `yaml:"key,omitempty"`
	// Timeout of one request; default 2m.
	Timeout Duration `yaml:"timeout,omitempty"`
	// Start launches a local server that does not answer; the last user stops it.
	Start []string `yaml:"start,omitempty"`
	// Ready is how long a started server may take to answer; default 10m.
	Ready Duration `yaml:"ready,omitempty"`
	// Health answers 200 when the server is up; default the endpoint's sibling /version.
	Health string `yaml:"health,omitempty"`
}

// Key says where the bearer token comes from: the first non-empty of Env, then Command's output.
type Key struct {
	Env     []string `yaml:"env,omitempty"`
	Command []string `yaml:"command,omitempty"`
}

// Alert runs Command, with {text} replaced, at the first refusal of a kind in an outage.
type Alert struct {
	Command []string `yaml:"command,omitempty"`
	// Texts per kind (credit, key); {provider} and {message} are replaced.
	Texts map[string]string `yaml:"texts,omitempty"`
}

var defaultTexts = map[string]string{
	KindCredit: "Jev : crédit épuisé, recharger sur console.typesafe.ai",
	KindKey:    "Jev : clé refusée par {provider} ({message}), vérifier la clé API",
}

// Duration reads "1h", "90s", "10min".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// ParseDuration is time.ParseDuration that also takes min and d.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "min"); ok {
		s = n + "m"
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err == nil {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}

// Path is the configuration file: $JEV_CONFIG, else ~/.config/jev/config.yaml.
func Path() string {
	if p := os.Getenv("JEV_CONFIG"); p != "" {
		return Expand(p)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "jev", "config.yaml")
}

// Load reads the configuration and opens a client.
func Load() (*Client, error) {
	cfg, err := LoadConfig(Path())
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

// LoadConfig reads a configuration file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("jev: no configuration at %s (providers, cooldown, alert)", path)
		}
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("jev: %s: %w", path, err)
	}
	c.path = path
	return &c, c.normalize()
}

// File is the file the configuration was read from.
func (c *Config) File() string { return c.path }

func (c *Config) normalize() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("jev: no provider in %s", c.path)
	}
	seen := map[string]bool{}
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Name == "" || p.URL == "" {
			return fmt.Errorf("jev: provider %d needs a name and a url", i+1)
		}
		if seen[p.Name] {
			return fmt.Errorf("jev: provider %q appears twice", p.Name)
		}
		seen[p.Name] = true
		if p.Timeout == 0 {
			p.Timeout = Duration(2 * time.Minute)
		}
		if p.Ready == 0 {
			p.Ready = Duration(10 * time.Minute)
		}
		for k := range p.Start {
			p.Start[k] = Expand(p.Start[k])
		}
		for k := range p.Key.Command {
			p.Key.Command[k] = Expand(p.Key.Command[k])
		}
	}
	if c.Cooldown == 0 {
		c.Cooldown = Duration(time.Hour)
	}
	if d := os.Getenv("JEV_STATE_DIR"); d != "" {
		c.StateDir = d
	}
	if c.StateDir == "" {
		home, _ := os.UserHomeDir()
		c.StateDir = filepath.Join(home, ".local", "state", "jev")
	}
	c.StateDir = Expand(c.StateDir)
	return nil
}

// Expand replaces a leading ~ with the home directory.
func Expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

func (c *Config) provider(name string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

func (c *Config) alertText(kind, provider, message string) string {
	t, ok := c.Alert.Texts[kind]
	if !ok {
		t = defaultTexts[kind]
	}
	return strings.NewReplacer("{provider}", provider, "{message}", message).Replace(t)
}
