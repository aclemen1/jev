package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Request is one call: the state and the questions of /v1/systemone.
type Request struct {
	State     any // a string, or an object
	Questions any // name → {type, instructions, criteria, …}
	// Only restricts the call to one provider, without fallback and despite its cooldown.
	Only string
}

// Answer is a provider's reply.
type Answer struct {
	Model   string                     `json:"model,omitempty"`
	Answers map[string]json.RawMessage `json:"answers"`
}

// Decode reads the answer to one question into v.
func (a Answer) Decode(question string, v any) error {
	raw, ok := a.Answers[question]
	if !ok {
		return fmt.Errorf("jev: no answer to %q", question)
	}
	return json.Unmarshal(raw, v)
}

// Attempt is one provider's failure.
type Attempt struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Status   int    `json:"status,omitempty"`
	Message  string `json:"message"`
}

func (a Attempt) String() string {
	s := a.Provider + ": " + a.Kind
	if a.Status != 0 {
		s += fmt.Sprintf(" (HTTP %d)", a.Status)
	}
	if a.Message != "" {
		s += ", " + a.Message
	}
	return s
}

// Error is returned when no provider answered.
type Error struct{ Attempts []Attempt }

func (e *Error) Error() string {
	if len(e.Attempts) == 0 {
		return "jev: no provider to ask"
	}
	parts := make([]string, len(e.Attempts))
	for i, a := range e.Attempts {
		parts[i] = a.String()
	}
	return "jev: no provider answered: " + strings.Join(parts, "; ")
}

// Client asks the providers of a configuration. Safe for concurrent use.
type Client struct {
	Config *Config
	// Warn receives what does not stop a call (an alert that failed); default stderr.
	Warn func(string)
	// Now is the clock; default time.Now.
	Now  func() time.Time
	HTTP *http.Client
	// NoStart uses a local server only when it already answers: for a caller that cannot wait for a start.
	NoStart bool

	mu    sync.Mutex
	keys  map[string]string
	pools map[string]*pool
}

// New opens a client on a configuration.
func New(cfg *Config) (*Client, error) {
	if cfg == nil || len(cfg.Providers) == 0 {
		return nil, errors.New("jev: no provider")
	}
	return &Client{Config: cfg, keys: map[string]string{}, pools: map[string]*pool{}}, nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) warn(s string) {
	if c.Warn != nil {
		c.Warn(s)
		return
	}
	fmt.Fprintln(os.Stderr, "jev: "+s)
}

// Close withdraws from the local servers this client used; the last user stops them.
func (c *Client) Close() error {
	c.mu.Lock()
	pools := c.pools
	c.pools = map[string]*pool{}
	c.mu.Unlock()
	for _, q := range pools {
		q.release()
	}
	return nil
}

// Ask asks the providers in order and returns the first answer with the
// provider that gave it. A provider in cooldown is skipped; a refusal for
// credit or key starts its cooldown and alerts once per outage; a success of
// the provider clears its cooldown and ends its outages.
func (c *Client) Ask(ctx context.Context, r Request) (Answer, string, error) {
	providers := c.Config.Providers
	if r.Only != "" {
		p, ok := c.Config.provider(r.Only)
		if !ok {
			return Answer{}, "", fmt.Errorf("jev: no provider %q in %s", r.Only, c.Config.File())
		}
		providers = []Provider{p}
	}
	state := c.Config.ReadState()
	var e Error
	for _, p := range providers {
		if cd, ok := state.Cooldowns[p.Name]; ok && r.Only == "" && c.now().Before(cd.Until) {
			e.Attempts = append(e.Attempts, Attempt{Provider: p.Name, Kind: cd.Kind, Status: cd.Status,
				Message: "cooldown until " + cd.Until.Local().Format("15:04") + " (jev reset to retry now)"})
			continue
		}
		a, att := c.askOne(ctx, p, r)
		if att == nil {
			if _, ok := state.Cooldowns[p.Name]; ok {
				_ = c.Config.updateState(func(s *State) bool { delete(s.Cooldowns, p.Name); return true })
			}
			for _, o := range c.Config.ReadAlerts().Open {
				if o.Provider == p.Name {
					if err := c.Config.endOutages(p.Name); err != nil {
						c.warn("outage not closed: " + err.Error())
					}
					break
				}
			}
			return a, p.Name, nil
		}
		e.Attempts = append(e.Attempts, *att)
		if ctx.Err() != nil {
			break
		}
		if att.Kind == KindCredit || att.Kind == KindKey {
			c.refused(p, *att)
		}
	}
	return Answer{}, "", &e
}

// refused starts a provider's cooldown and sends the alert of the kind, once per outage.
func (c *Client) refused(p Provider, a Attempt) {
	now := c.now()
	if err := c.Config.updateState(func(s *State) bool {
		since := now
		if old, ok := s.Cooldowns[p.Name]; ok && old.Kind == a.Kind {
			since = old.Since
		}
		s.Cooldowns[p.Name] = Cooldown{Kind: a.Kind, Status: a.Status, Message: a.Message, Since: since, Until: now.Add(time.Duration(c.Config.Cooldown))}
		return true
	}); err != nil {
		c.warn("cooldown not recorded: " + err.Error())
	}
	if len(c.Config.Alert.Command) == 0 {
		return
	}
	claimed, err := c.Config.claimAlert(a.Kind, p.Name, now)
	if err != nil {
		c.warn("alert not sent: " + err.Error())
		return
	}
	if !claimed {
		return
	}
	text := c.Config.alertText(a.Kind, p.Name, truncate(a.Message, 160))
	sa := SentAlert{Kind: a.Kind, Provider: p.Name, At: now, Text: text}
	if err := runAlert(c.Config.Alert.Command, text); err != nil {
		sa.Error = err.Error()
		c.warn("alert failed: " + err.Error())
	}
	c.Config.recordAlert(sa)
}

func runAlert(command []string, text string) error {
	args := make([]string, len(command))
	for i, a := range command {
		args[i] = strings.ReplaceAll(Expand(a), "{text}", text)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v %s", args[0], err, truncate(strings.TrimSpace(string(out)), 200))
	}
	return nil
}

func (c *Client) askOne(ctx context.Context, p Provider, r Request) (Answer, *Attempt) {
	fail := func(kind string, status int, msg string) (Answer, *Attempt) {
		return Answer{}, &Attempt{Provider: p.Name, Kind: kind, Status: status, Message: msg}
	}
	if len(p.Start) > 0 && c.NoStart {
		if !Up(p) {
			return fail(KindUnavailable, 0, "not running, and this caller does not start it")
		}
	} else if len(p.Start) > 0 {
		q := c.pool(p)
		if err := q.acquire(ctx); err != nil {
			return fail(KindUnavailable, 0, err.Error())
		}
	}
	key, err := c.key(p)
	if err != nil {
		return fail(KindKey, 0, err.Error())
	}
	body, err := json.Marshal(map[string]any{"model": p.Model, "state": r.State, "questions": r.Questions})
	if err != nil {
		return Answer{}, &Attempt{Provider: p.Name, Kind: KindUnavailable, Message: err.Error()}
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(p.Timeout))
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return fail(KindUnavailable, 0, err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return fail(KindUnavailable, 0, "no answer after "+time.Duration(p.Timeout).String())
		}
		return fail(KindUnavailable, 0, err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fail(KindUnavailable, resp.StatusCode, err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		kind, msg := Classify(resp.StatusCode, raw)
		return fail(kind, resp.StatusCode, msg)
	}
	var out struct {
		Answer
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fail(KindUnavailable, resp.StatusCode, "unreadable answer: "+truncate(string(raw), 200))
	}
	if len(out.Error) > 0 && string(out.Error) != "null" {
		return fail(KindUnavailable, resp.StatusCode, errorMessage(raw))
	}
	if len(out.Answers) == 0 {
		return fail(KindUnavailable, resp.StatusCode, "no answers in the reply")
	}
	return out.Answer, nil
}

func (c *Client) pool(p Provider) *pool {
	c.mu.Lock()
	defer c.mu.Unlock()
	q, ok := c.pools[p.Name]
	if !ok {
		q = &pool{cfg: c.Config, p: p}
		c.pools[p.Name] = q
	}
	return q
}

// key reads a provider's bearer token once: the first non-empty of key.env, then key.command.
func (c *Client) key(p Provider) (string, error) {
	if len(p.Key.Env) == 0 && len(p.Key.Command) == 0 {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if k, ok := c.keys[p.Name]; ok {
		return k, nil
	}
	for _, e := range p.Key.Env {
		if v := strings.TrimSpace(os.Getenv(e)); v != "" {
			c.keys[p.Name] = v
			return v, nil
		}
	}
	if len(p.Key.Command) > 0 {
		out, err := exec.Command(p.Key.Command[0], p.Key.Command[1:]...).Output()
		if v := strings.TrimSpace(string(out)); err == nil && v != "" {
			c.keys[p.Name] = v
			return v, nil
		}
		if err != nil {
			return "", fmt.Errorf("no key: key.command failed (%v)", err)
		}
	}
	return "", fmt.Errorf("no key: set $%s or key.command", strings.Join(p.Key.Env, " or $"))
}
