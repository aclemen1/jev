package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Cooldown is a provider skipped after a refusal for credit or key.
type Cooldown struct {
	Kind    string    `json:"kind"`
	Status  int       `json:"status,omitempty"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
}

// State is state.json: the cooldowns, by provider.
type State struct {
	Cooldowns map[string]Cooldown `json:"cooldowns"`
}

// SentAlert is the last alert sent, or tried.
type SentAlert struct {
	Kind     string    `json:"kind"`
	Provider string    `json:"provider"`
	At       time.Time `json:"at"`
	Text     string    `json:"text"`
	Error    string    `json:"error,omitempty"`
}

func (c *Config) statePath() string  { return filepath.Join(c.StateDir, "state.json") }
func (c *Config) alertsPath() string { return filepath.Join(c.StateDir, "alerts.json") }

// lockFile holds an exclusive flock on path+".lock" while fn runs.
func lockFile(ctx context.Context, path string, timeout time.Duration, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("jev: %s stays locked after %s", path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func readJSON(path string, v any) {
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, v)
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// updateState changes state.json under its lock.
func (c *Config) updateState(fn func(*State) bool) error {
	p := c.statePath()
	return lockFile(context.Background(), p, 10*time.Second, func() error {
		var s State
		readJSON(p, &s)
		if s.Cooldowns == nil {
			s.Cooldowns = map[string]Cooldown{}
		}
		if !fn(&s) {
			return nil
		}
		return writeJSON(p, s)
	})
}

// ReadState reads state.json; a missing file gives no cooldown.
func (c *Config) ReadState() State {
	var s State
	readJSON(c.statePath(), &s)
	if s.Cooldowns == nil {
		s.Cooldowns = map[string]Cooldown{}
	}
	return s
}

// Outage is a refusal of a kind already alerted: no new alert until a success of its provider.
type Outage struct {
	Provider string    `json:"provider"`
	Since    time.Time `json:"since"`
}

// Alerts is alerts.json: the outages already alerted, by kind, and the last alert.
type Alerts struct {
	Open map[string]Outage `json:"open"` // kind → outage
	Last *SentAlert        `json:"last,omitempty"`
}

func (c *Config) updateAlerts(fn func(*Alerts) bool) error {
	p := c.alertsPath()
	return lockFile(context.Background(), p, 10*time.Second, func() error {
		var a Alerts
		readJSON(p, &a)
		if a.Open == nil {
			a.Open = map[string]Outage{}
		}
		if !fn(&a) {
			return nil
		}
		return writeJSON(p, a)
	})
}

// ReadAlerts reads alerts.json.
func (c *Config) ReadAlerts() Alerts {
	var a Alerts
	readJSON(c.alertsPath(), &a)
	if a.Open == nil {
		a.Open = map[string]Outage{}
	}
	return a
}

// claimAlert says whether this outage of the kind is not alerted yet, and marks it alerted.
func (c *Config) claimAlert(kind, provider string, now time.Time) (bool, error) {
	claimed := false
	err := c.updateAlerts(func(a *Alerts) bool {
		if _, open := a.Open[kind]; open {
			return false
		}
		a.Open[kind] = Outage{Provider: provider, Since: now}
		claimed = true
		return true
	})
	return claimed, err
}

// endOutages closes the outages of a provider that answered: a new refusal alerts again.
func (c *Config) endOutages(provider string) error {
	return c.updateAlerts(func(a *Alerts) bool {
		changed := false
		for k, o := range a.Open {
			if o.Provider == provider {
				delete(a.Open, k)
				changed = true
			}
		}
		return changed
	})
}

func (c *Config) recordAlert(sa SentAlert) {
	_ = c.updateAlerts(func(a *Alerts) bool { a.Last = &sa; return true })
}

// Reset clears every cooldown and every open outage, so the next question
// goes to the first provider and a new refusal alerts again.
func (c *Config) Reset(now time.Time) (cleared []string, err error) {
	err = c.updateState(func(s *State) bool {
		for name := range s.Cooldowns {
			cleared = append(cleared, name)
		}
		s.Cooldowns = map[string]Cooldown{}
		return true
	})
	if err != nil {
		return nil, err
	}
	err = c.updateAlerts(func(a *Alerts) bool { a.Open = map[string]Outage{}; return true })
	return cleared, err
}
