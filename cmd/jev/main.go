// Command jev shows and resets the state of the Jev providers.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aclemen1/jev"
)

const usage = `jev status [--format json|text]   provider asked first, cooldowns, open outages, last alert
jev reset  [--format json|text]   clear the cooldowns and the outages: Jev is asked again now, a new refusal alerts again

Configuration: $JEV_CONFIG, else ~/.config/jev/config.yaml.`

type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, time.Now()))
}

func run(args []string, w io.Writer, now time.Time) int {
	format := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--format" && i+1 < len(args):
			format = args[i+1]
			i++
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		case a == "-h" || a == "--help" || a == "help":
			fmt.Fprintln(w, usage)
			return 0
		default:
			rest = append(rest, a)
		}
	}
	if format == "" {
		format = "text"
		if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
			format = "json"
		}
	}
	if format != "json" && format != "text" {
		return fail(w, "text", userError{fmt.Sprintf("--format takes json or text, not %q", format)})
	}
	if len(rest) != 1 {
		return fail(w, format, userError{"one command: status or reset\n" + usage})
	}
	cfg, err := jev.LoadConfig(jev.Path())
	if err != nil {
		return fail(w, format, userError{err.Error()})
	}
	switch rest[0] {
	case "status":
		st := cfg.Status(now)
		return ok(w, format, st, statusText(st, now))
	case "reset":
		cleared, err := cfg.Reset(now)
		if err != nil {
			return fail(w, format, err)
		}
		if cleared == nil {
			cleared = []string{}
		}
		st := cfg.Status(now)
		text := "No cooldown. "
		if len(cleared) > 0 {
			text = "Cooldown cleared: " + strings.Join(cleared, ", ") + ". "
		}
		text += "Outages cleared. Next question goes to " + st.Active + "."
		return ok(w, format, map[string]any{"cleared": cleared, "active": st.Active}, text)
	}
	return fail(w, format, userError{fmt.Sprintf("unknown command %q\n%s", rest[0], usage)})
}

func ok(w io.Writer, format string, result any, text string) int {
	if format == "json" {
		b, _ := json.Marshal(map[string]any{"ok": true, "result": result})
		fmt.Fprintln(w, string(b))
	} else {
		fmt.Fprintln(w, text)
	}
	return 0
}

func fail(w io.Writer, format string, err error) int {
	code, kind := 1, "internal_error"
	if _, user := err.(userError); user {
		code, kind = 2, "user_error"
	}
	if format == "json" {
		b, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"code": code, "kind": kind, "message": err.Error()}})
		fmt.Fprintln(w, string(b))
	} else {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
	}
	return code
}

func statusText(st jev.Status, now time.Time) string {
	var b strings.Builder
	active := st.Active
	if active == "" {
		active = "none (all in cooldown)"
	}
	fmt.Fprintf(&b, "Asked first: %s\n", active)
	for _, p := range st.Providers {
		fmt.Fprintf(&b, "  %-8s %s", p.Name, p.URL)
		if p.Cooldown != nil {
			fmt.Fprintf(&b, "  cooldown (%s) until %s, %s left", p.Cooldown.Kind, p.Cooldown.Until.Local().Format("15:04"), p.Cooldown.Until.Sub(now).Round(time.Minute))
			if p.Cooldown.Message != "" {
				fmt.Fprintf(&b, ": %s", p.Cooldown.Message)
			}
		}
		if p.Local != nil {
			state := "down (started on demand)"
			if p.Local.Up {
				state = fmt.Sprintf("up, %d user(s)", p.Local.Users)
			}
			fmt.Fprintf(&b, "  [%s]", state)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Cooldown after a refusal: %s\n", st.Cooldown)
	if st.LastAlert != nil {
		a := st.LastAlert
		fmt.Fprintf(&b, "Last alert: %s, %s: %s", a.At.Local().Format("2006-01-02 15:04"), a.Kind, a.Text)
		if a.Error != "" {
			fmt.Fprintf(&b, " (failed: %s)", a.Error)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Last alert: none\n")
	}
	for _, k := range []string{jev.KindCredit, jev.KindKey} {
		if o, ok := st.Outages[k]; ok {
			fmt.Fprintf(&b, "Outage (%s) of %s since %s, alerted: no new alert until it answers (or jev reset)\n", k, o.Provider, o.Since.Local().Format("2006-01-02 15:04"))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
