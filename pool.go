package jev

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A pool shares a local server between processes: the first user that finds
// it down starts it, every user registers, the last one leaving stops it, and
// only when a user started it.
type pool struct {
	cfg  *Config
	p    Provider
	id   string // this client's registration
	held bool
}

type holder struct {
	PID int    `json:"pid"`
	ID  string `json:"id"`
}

type poolState struct {
	Holders []holder `json:"holders"`
	Started int      `json:"started,omitempty"` // pid (and process group) of the server a user started
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func (q *pool) path() string { return filepath.Join(q.cfg.StateDir, "pool-"+q.p.Name+".json") }

func (q *pool) logPath() string { return filepath.Join(q.cfg.StateDir, q.p.Name+".log") }

// healthURL is the endpoint's sibling /version: …/v1/systemone → …/v1/version.
func healthURL(p Provider) string {
	if p.Health != "" {
		return p.Health
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return p.URL
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if i := strings.LastIndex(u.Path, "/"); i >= 0 {
		u.Path = u.Path[:i+1]
	}
	u.Path += "version"
	return u.String()
}

// Up says whether a provider's server answers its health check.
func Up(p Provider) bool {
	r, err := (&http.Client{Timeout: 3 * time.Second}).Get(healthURL(p))
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.StatusCode == http.StatusOK
}

// acquire registers this client, starting the server when it does not answer.
func (q *pool) acquire(ctx context.Context) error {
	if q.held && Up(q.p) {
		return nil
	}
	if q.id == "" {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		q.id = hex.EncodeToString(b)
	}
	ready := time.Duration(q.p.Ready)
	return lockFile(ctx, q.path(), ready+time.Minute, func() error {
		var s poolState
		readJSON(q.path(), &s)
		var live []holder
		for _, h := range s.Holders {
			if alive(h.PID) && h.ID != q.id {
				live = append(live, h)
			}
		}
		s.Holders = live
		if s.Started != 0 && !alive(s.Started) {
			s.Started = 0
		}
		if !Up(q.p) {
			if s.Started != 0 { // started by a user, but not answering: start again
				_ = syscall.Kill(-s.Started, syscall.SIGTERM)
				s.Started = 0
			}
			pid, err := q.launch()
			if err != nil {
				_ = writeJSON(q.path(), s)
				return err
			}
			if err := q.waitUp(ctx, pid, ready); err != nil {
				_ = syscall.Kill(-pid, syscall.SIGTERM)
				_ = writeJSON(q.path(), s)
				return err
			}
			s.Started = pid
		}
		s.Holders = append(s.Holders, holder{PID: os.Getpid(), ID: q.id})
		q.held = true
		return writeJSON(q.path(), s)
	})
}

// launch runs the start command in its own session, so that it outlives this process.
func (q *pool) launch() (int, error) {
	if len(q.p.Start) == 0 {
		return 0, fmt.Errorf("%s: not answering at %s", q.p.Name, healthURL(q.p))
	}
	cmd := exec.Command(q.p.Start[0], q.p.Start[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if f, err := os.OpenFile(q.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		cmd.Stdout, cmd.Stderr = f, f
		defer f.Close()
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("%s: start: %w", q.p.Name, err)
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

func (q *pool) waitUp(ctx context.Context, pid int, ready time.Duration) error {
	deadline := time.Now().Add(ready)
	for time.Now().Before(deadline) {
		if Up(q.p) {
			return nil
		}
		if !alive(pid) {
			return fmt.Errorf("%s: the server stopped while starting (log: %s)", q.p.Name, q.logPath())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s: not ready after %s (log: %s)", q.p.Name, ready, q.logPath())
}

// release withdraws this client; the last user stops a server a user started.
func (q *pool) release() {
	if !q.held {
		return
	}
	q.held = false
	_ = lockFile(context.Background(), q.path(), 30*time.Second, func() error {
		var s poolState
		readJSON(q.path(), &s)
		var live []holder
		for _, h := range s.Holders {
			if alive(h.PID) && h.ID != q.id {
				live = append(live, h)
			}
		}
		s.Holders = live
		if len(live) == 0 && s.Started != 0 {
			pid := s.Started
			_ = syscall.Kill(-pid, syscall.SIGTERM)
			for i := 0; i < 50 && alive(pid); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			s.Started = 0
		}
		return writeJSON(q.path(), s)
	})
}

// PoolInfo describes a shared local server.
type PoolInfo struct {
	Up      bool `json:"up"`
	Users   int  `json:"users"`
	Started int  `json:"started,omitempty"`
}

func (c *Config) poolInfo(p Provider) PoolInfo {
	q := &pool{cfg: c, p: p}
	var s poolState
	readJSON(q.path(), &s)
	n := 0
	for _, h := range s.Holders {
		if alive(h.PID) {
			n++
		}
	}
	if s.Started != 0 && !alive(s.Started) {
		s.Started = 0
	}
	return PoolInfo{Up: Up(p), Users: n, Started: s.Started}
}
