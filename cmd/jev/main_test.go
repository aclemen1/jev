package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnvelope(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("providers:\n  - {name: jev, url: http://127.0.0.1:1/v1/systemone}\n  - {name: openjev, url: http://127.0.0.1:2/v1/systemone}\nstate_dir: "+dir+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"cooldowns":{"jev":{"kind":"credit","until":"2999-01-01T00:00:00Z"}}}`), 0o644)
	t.Setenv("JEV_CONFIG", cfg)
	var out struct {
		OK     bool
		Result map[string]any
		Error  map[string]any
	}
	call := func(args ...string) int {
		var b bytes.Buffer
		code := run(append(args, "--format", "json"), &b, time.Now())
		out.Result, out.Error = nil, nil
		if err := json.Unmarshal(b.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", b.String(), err)
		}
		return code
	}
	if call("status"); !out.OK || out.Result["active"] != "openjev" {
		t.Fatalf("status %+v", out)
	}
	if call("reset"); !out.OK || out.Result["active"] != "jev" {
		t.Fatalf("reset %+v", out)
	}
	if code := call("nope"); code != 2 || out.OK || out.Error["kind"] != "user_error" {
		t.Fatalf("error %+v", out)
	}
}
