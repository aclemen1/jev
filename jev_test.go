package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   string
	}{
		{401, `{"detail":{"error_type":"authentication_error","message":"Invalid API key"}}`, KindKey},
		{402, `{"detail":"Payment required"}`, KindCredit},
		{403, `{"detail":{"error_type":"permission_error","message":"Insufficient credit balance"}}`, KindCredit},
		{403, `{"detail":{"error_type":"permission_error","message":"Forbidden"}}`, KindKey},
		{429, `{"detail":{"error_type":"rate_limit_error","message":"slow down","retry_after_ms":500}}`, KindUnavailable},
		{429, `{"error":{"type":"quota_exceeded","message":"monthly quota reached"}}`, KindCredit},
		{500, `oops`, KindUnavailable},
		{503, `{"detail":"overloaded"}`, KindUnavailable},
		{422, `{"detail":[{"loc":["body","state"],"msg":"field required"}]}`, KindUnavailable},
	}
	for _, c := range cases {
		kind, msg := Classify(c.status, []byte(c.body))
		if kind != c.kind {
			t.Errorf("%d %s: kind %s (message %q), want %s", c.status, c.body, kind, msg, c.kind)
		}
	}
	if _, msg := Classify(401, []byte(`{"detail":{"error_type":"authentication_error","message":"Invalid API key"}}`)); msg != "authentication_error: Invalid API key" {
		t.Errorf("message %q", msg)
	}
}

// fake is a /v1/systemone server whose answer the test sets.
type fake struct {
	*httptest.Server
	status atomic.Int32
	body   atomic.Value
	delay  atomic.Int64
	hits   atomic.Int32
	auth   atomic.Value
}

func newFake(t *testing.T, model string) *fake {
	f := &fake{}
	f.status.Store(200)
	f.body.Store(`{"model":"` + model + ` 1.0","answers":{"q":{"choice":"a","confidence":0.9}}}`)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/version" {
			w.Write([]byte(`{"version":"test"}`))
			return
		}
		f.hits.Add(1)
		f.auth.Store(r.Header.Get("Authorization"))
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req["model"] == nil || req["questions"] == nil {
			w.WriteHeader(400)
			return
		}
		if d := f.delay.Load(); d > 0 {
			select {
			case <-time.After(time.Duration(d)):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(int(f.status.Load()))
		w.Write([]byte(f.body.Load().(string)))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) refuse(status int, body string) { f.status.Store(int32(status)); f.body.Store(body) }

type env struct {
	c      *Client
	jev    *fake
	local  *fake
	alerts string
	clock  time.Time
	warns  []string
}

func setup(t *testing.T) *env {
	dir := t.TempDir()
	e := &env{jev: newFake(t, "jev-latest"), local: newFake(t, "openjev"), alerts: filepath.Join(dir, "alerts.txt"),
		clock: time.Date(2026, 10, 11, 10, 0, 0, 0, time.Local)}
	t.Setenv("JEV_TEST_KEY", "sk-test")
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
providers:
  - name: jev
    url: %s/v1/systemone
    model: jev-latest
    key: {env: [JEV_TEST_KEY]}
    timeout: 300ms
  - name: openjev
    url: %s/v1/systemone
    model: openjev
cooldown: 1h
alert:
  command: [sh, -c, 'printf "%%s\n" "$0" >> %s', "{text}"]
state_dir: %s
`, e.jev.URL, e.local.URL, e.alerts, filepath.Join(dir, "state"))), 0o644)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.Now = func() time.Time { return e.clock }
	c.Warn = func(s string) { e.warns = append(e.warns, s) }
	t.Cleanup(func() { c.Close() })
	e.c = c
	return e
}

func (e *env) ask(t *testing.T) string {
	t.Helper()
	a, p, err := e.c.Ask(context.Background(), Request{State: "s", Questions: map[string]any{"q": map[string]any{"type": "choice"}}})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	var q struct{ Choice string }
	if err := a.Decode("q", &q); err != nil || q.Choice != "a" {
		t.Fatalf("answer %+v: %v", a, err)
	}
	return p
}

func (e *env) sent(t *testing.T) []string {
	b, _ := os.ReadFile(e.alerts)
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestJevAnswersWithKey(t *testing.T) {
	e := setup(t)
	if p := e.ask(t); p != "jev" {
		t.Fatalf("provider %s", p)
	}
	if a := e.jev.auth.Load(); a != "Bearer sk-test" {
		t.Fatalf("auth %v", a)
	}
	if e.local.hits.Load() != 0 {
		t.Fatal("openjev asked")
	}
}

func TestCreditFallbackCooldownAndOneAlert(t *testing.T) {
	e := setup(t)
	e.jev.refuse(402, `{"detail":{"error_type":"payment_required","message":"Insufficient credit"}}`)
	if p := e.ask(t); p != "openjev" {
		t.Fatalf("provider %s", p)
	}
	if p := e.ask(t); p != "openjev" {
		t.Fatalf("provider %s", p)
	}
	if n := e.jev.hits.Load(); n != 1 {
		t.Fatalf("jev asked %d times during cooldown", n)
	}
	got := e.sent(t)
	if len(got) != 1 || got[0] != "Jev : crédit épuisé, recharger sur console.typesafe.ai" {
		t.Fatalf("alerts %q", got)
	}
	cd := e.c.Config.ReadState().Cooldowns["jev"]
	if cd.Kind != KindCredit || !cd.Until.Equal(e.clock.Add(time.Hour)) {
		t.Fatalf("cooldown %+v", cd)
	}
	// After the cooldown, still refused: tried again, same outage, no new alert.
	e.clock = e.clock.Add(61 * time.Minute)
	e.jev.refuse(403, `{"detail":{"error_type":"permission_error","message":"Account balance is zero"}}`)
	if p := e.ask(t); p != "openjev" || e.jev.hits.Load() != 2 {
		t.Fatalf("provider %s, jev hits %d", p, e.jev.hits.Load())
	}
	// Next day, still the same outage: no new alert.
	e.clock = e.clock.Add(24 * time.Hour)
	e.ask(t)
	if got := e.sent(t); len(got) != 1 {
		t.Fatalf("alerts %q", got)
	}
	// Recharged, cooldown over: back to jev, cooldown and outage cleared.
	e.clock = e.clock.Add(2 * time.Hour)
	e.jev.refuse(200, `{"answers":{"q":{"choice":"a","confidence":1}}}`)
	if p := e.ask(t); p != "jev" {
		t.Fatalf("provider %s", p)
	}
	if n := len(e.c.Config.ReadState().Cooldowns); n != 0 {
		t.Fatalf("%d cooldowns left", n)
	}
	if n := len(e.c.Config.ReadAlerts().Open); n != 0 {
		t.Fatalf("%d outages left", n)
	}
	// A new outage, the same day: a second alert, and only one.
	e.clock = e.clock.Add(time.Minute)
	e.jev.refuse(402, `{}`)
	e.ask(t)
	e.clock = e.clock.Add(2 * time.Hour)
	e.ask(t)
	if got := e.sent(t); len(got) != 2 {
		t.Fatalf("alerts %q", got)
	}
}

func TestKeyOutageEndsOnSuccess(t *testing.T) {
	e := setup(t)
	e.jev.refuse(401, `{"detail":{"error_type":"authentication_error","message":"Invalid API key"}}`)
	e.ask(t)
	e.clock = e.clock.Add(2 * time.Hour)
	e.ask(t) // same outage
	if got := e.sent(t); len(got) != 1 {
		t.Fatalf("alerts %q", got)
	}
	e.clock = e.clock.Add(2 * time.Hour)
	e.jev.refuse(200, `{"answers":{"q":{"choice":"a"}}}`)
	if p := e.ask(t); p != "jev" {
		t.Fatalf("provider %s", p)
	}
	e.jev.refuse(401, `{"detail":"bad key"}`)
	e.ask(t)
	if got := e.sent(t); len(got) != 2 || !strings.Contains(got[1], "clé") {
		t.Fatalf("alerts %q", got)
	}
}

func TestOpenJevSuccessDoesNotEndJevOutage(t *testing.T) {
	e := setup(t)
	e.jev.refuse(402, `{}`)
	e.ask(t) // openjev answers
	if _, ok := e.c.Config.ReadAlerts().Open[KindCredit]; !ok {
		t.Fatal("outage closed by openjev's answer")
	}
}

func TestKeyAlertIsDistinct(t *testing.T) {
	e := setup(t)
	e.jev.refuse(402, `{"detail":"Payment required"}`)
	e.ask(t)
	e.clock = e.clock.Add(2 * time.Hour)
	e.jev.refuse(401, `{"detail":{"error_type":"authentication_error","message":"Invalid API key"}}`)
	e.ask(t)
	e.clock = e.clock.Add(2 * time.Hour)
	e.jev.refuse(403, `{"detail":{"error_type":"permission_error","message":"Forbidden"}}`)
	e.ask(t)
	got := e.sent(t)
	if len(got) != 2 || !strings.Contains(got[1], "clé refusée par jev") || !strings.Contains(got[1], "Invalid API key") {
		t.Fatalf("alerts %q", got)
	}
	if cd := e.c.Config.ReadState().Cooldowns["jev"]; cd.Kind != KindKey || cd.Status != 403 {
		t.Fatalf("cooldown %+v", cd)
	}
}

func TestUnavailableFallsBackWithoutAlert(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		delay  time.Duration
	}{
		{"429", 429, `{"detail":{"error_type":"rate_limit_error","message":"Too many requests","retry_after_ms":1000}}`, 0},
		{"500", 500, `{"detail":"Internal Server Error"}`, 0},
		{"timeout", 200, `{"answers":{"q":{"choice":"a"}}}`, time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			e.jev.refuse(c.status, c.body)
			e.jev.delay.Store(int64(c.delay))
			if p := e.ask(t); p != "openjev" {
				t.Fatalf("provider %s", p)
			}
			if p := e.ask(t); p != "openjev" || e.jev.hits.Load() != 2 {
				t.Fatalf("jev not tried again (hits %d)", e.jev.hits.Load())
			}
			if got := e.sent(t); len(got) != 0 {
				t.Fatalf("alerts %q", got)
			}
			if n := len(e.c.Config.ReadState().Cooldowns); n != 0 {
				t.Fatal("cooldown on an unavailable provider")
			}
		})
	}
}

func TestConnectionRefusedFallsBack(t *testing.T) {
	e := setup(t)
	e.jev.Close()
	if p := e.ask(t); p != "openjev" {
		t.Fatalf("provider %s", p)
	}
}

func TestResetReturnsToJevAndAllowsANewAlert(t *testing.T) {
	e := setup(t)
	e.jev.refuse(402, `{}`)
	e.ask(t)
	cleared, err := e.c.Config.Reset(e.clock)
	if err != nil || len(cleared) != 1 || cleared[0] != "jev" {
		t.Fatalf("reset %v %v", cleared, err)
	}
	e.ask(t) // refused again at once: asked, and alerted again
	if e.jev.hits.Load() != 2 {
		t.Fatalf("jev hits %d", e.jev.hits.Load())
	}
	if got := e.sent(t); len(got) != 2 {
		t.Fatalf("alerts %q", got)
	}
	e.jev.refuse(200, `{"answers":{"q":{"choice":"a"}}}`)
	e.c.Config.Reset(e.clock)
	if p := e.ask(t); p != "jev" {
		t.Fatalf("provider %s", p)
	}
	st := e.c.Config.Status(e.clock)
	if st.Active != "jev" || st.LastAlert == nil || st.LastAlert.Kind != KindCredit {
		t.Fatalf("status %+v", st)
	}
}

func TestStatusDuringCooldown(t *testing.T) {
	e := setup(t)
	e.jev.refuse(402, `{}`)
	e.ask(t)
	st := e.c.Config.Status(e.clock)
	if st.Active != "openjev" || st.Providers[0].Cooldown == nil || len(st.Outages) != 1 {
		t.Fatalf("status %+v", st)
	}
}

func TestAlertFailureDoesNotBlock(t *testing.T) {
	e := setup(t)
	e.c.Config.Alert.Command = []string{"/nonexistent/alert", "{text}"}
	e.jev.refuse(402, `{}`)
	if p := e.ask(t); p != "openjev" {
		t.Fatalf("provider %s", p)
	}
	if len(e.warns) != 1 || !strings.Contains(e.warns[0], "alert failed") {
		t.Fatalf("warns %q", e.warns)
	}
	if a := e.c.Config.ReadAlerts().Last; a == nil || a.Error == "" {
		t.Fatalf("last alert %+v", a)
	}
}

func TestAllFail(t *testing.T) {
	e := setup(t)
	e.jev.refuse(402, `{}`)
	e.local.refuse(500, `{}`)
	_, _, err := e.c.Ask(context.Background(), Request{State: "s", Questions: map[string]any{}})
	var je *Error
	if !errors.As(err, &je) || len(je.Attempts) != 2 || je.Attempts[0].Kind != KindCredit || je.Attempts[1].Kind != KindUnavailable {
		t.Fatalf("err %v", err)
	}
	// In cooldown, the attempt says so.
	_, _, err = e.c.Ask(context.Background(), Request{State: "s", Questions: map[string]any{}})
	if !strings.Contains(err.Error(), "cooldown until") {
		t.Fatalf("err %v", err)
	}
}

func TestOnlyIgnoresCooldownAndFallback(t *testing.T) {
	e := setup(t)
	e.jev.refuse(500, `{}`)
	if _, _, err := e.c.Ask(context.Background(), Request{State: "s", Questions: map[string]any{}, Only: "jev"}); err == nil {
		t.Fatal("no error")
	}
	if e.local.hits.Load() != 0 {
		t.Fatal("fell back despite Only")
	}
	a, p, err := e.c.Ask(context.Background(), Request{State: "s", Questions: map[string]any{}, Only: "openjev"})
	if err != nil || p != "openjev" || a.Model != "openjev 1.0" {
		t.Fatalf("%v %s %v", a, p, err)
	}
}

// TestHelperServer is the local server the pool test starts.
func TestHelperServer(t *testing.T) {
	port := os.Getenv("JEV_HELPER_PORT")
	if port == "" {
		t.Skip("helper process")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("/v1/systemone", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"model":"openjev","answers":{"q":{"choice":"a"}}}`))
	})
	http.ListenAndServe("127.0.0.1:"+port, mux)
}

func TestPoolStartsAndLastUserStops(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	t.Setenv("JEV_HELPER_PORT", port)
	dir := t.TempDir()
	cfg := &Config{StateDir: dir, Providers: []Provider{{Name: "openjev", URL: "http://127.0.0.1:" + port + "/v1/systemone",
		Model: "openjev", Start: []string{os.Args[0], "-test.run=^TestHelperServer$"}, Ready: Duration(20 * time.Second)}}, path: "test"}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	c1, _ := New(cfg)
	c2, _ := New(cfg)
	req := Request{State: "s", Questions: map[string]any{"q": map[string]any{}}}
	if _, p, err := c1.Ask(context.Background(), req); err != nil || p != "openjev" {
		t.Fatalf("c1: %s %v", p, err)
	}
	if _, _, err := c2.Ask(context.Background(), req); err != nil {
		t.Fatalf("c2: %v", err)
	}
	if info := cfg.poolInfo(cfg.Providers[0]); !info.Up || info.Users != 2 || info.Started == 0 {
		t.Fatalf("pool %+v", info)
	}
	c1.Close()
	if !Up(cfg.Providers[0]) {
		t.Fatal("stopped while c2 uses it")
	}
	c2.Close()
	deadline := time.Now().Add(5 * time.Second)
	for Up(cfg.Providers[0]) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if Up(cfg.Providers[0]) {
		t.Fatal("still up after the last user left")
	}
}

func TestPoolLeavesAServerItDidNotStart(t *testing.T) {
	f := newFake(t, "openjev")
	cfg := &Config{StateDir: t.TempDir(), Providers: []Provider{{Name: "openjev", URL: f.URL + "/v1/systemone", Start: []string{"/bin/false"}}}, path: "test"}
	cfg.normalize()
	c, _ := New(cfg)
	if _, _, err := c.Ask(context.Background(), Request{State: "s", Questions: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !Up(cfg.Providers[0]) {
		t.Fatal("stopped a server it did not start")
	}
}

func TestOneAlertPerOutageAcrossClients(t *testing.T) {
	e := setup(t)
	burst := func() {
		done := make(chan struct{})
		for i := 0; i < 8; i++ {
			go func() {
				c, _ := New(e.c.Config)
				clock := e.clock
				c.Now = func() time.Time { return clock }
				c.Ask(context.Background(), Request{State: "s", Questions: map[string]any{"q": map[string]any{}}})
				done <- struct{}{}
			}()
		}
		for i := 0; i < 8; i++ {
			<-done
		}
	}
	e.jev.refuse(402, `{}`)
	burst()
	if got := e.sent(t); len(got) != 1 {
		t.Fatalf("first outage: alerts %q", got)
	}
	e.clock = e.clock.Add(2 * time.Hour)
	e.jev.refuse(200, `{"answers":{"q":{"choice":"a"}}}`)
	burst() // jev back: outage over
	e.jev.refuse(402, `{}`)
	e.clock = e.clock.Add(time.Minute)
	burst()
	if got := e.sent(t); len(got) != 2 {
		t.Fatalf("second outage: alerts %q", got)
	}
}

func TestNoStartSkipsADownServer(t *testing.T) {
	cfg := &Config{StateDir: t.TempDir(), Providers: []Provider{{Name: "openjev", URL: "http://127.0.0.1:1/v1/systemone", Start: []string{"/bin/sh", "-c", "touch " + filepath.Join(t.TempDir(), "started")}}}, path: "test"}
	cfg.normalize()
	c, _ := New(cfg)
	c.NoStart = true
	_, _, err := c.Ask(context.Background(), Request{State: "s", Questions: map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "does not start it") {
		t.Fatalf("err %v", err)
	}
}
