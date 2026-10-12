> **Ce dépôt est archivé.** Depuis le 12.10.2026, le code vit dans le monorepo privé `aclemen1/jarvis`, sous `jev/`. Ce dépôt ne reçoit plus de changements.

# jev

Go client for TypeSafe's System One API (`POST /v1/systemone`): the hosted Jev
first, a local OpenJev when Jev refuses or does not answer.

```go
c, err := jev.Load() // ~/.config/jev/config.yaml, or $JEV_CONFIG
defer c.Close()      // the last user stops an OpenJev a user started
ans, provider, err := c.Ask(ctx, jev.Request{State: state, Questions: questions})
var node struct{ Choice string; Confidence float64 }
ans.Decode("node", &node)
```

`Request.Only` names one provider (no fallback, cooldown ignored).
`Client.NoStart` uses a local server only when it already answers.

## Failures

| Answer | Kind | Effect |
|---|---|---|
| 402; 403 or 429 whose message names credit, balance, quota, insufficient or payment | `credit` | cooldown, alert, next provider |
| 401; other 403 | `key` | cooldown, alert, next provider |
| 5xx, plain 429, other 4xx, connection, timeout | `unavailable` | next provider |

- Cooldown: the provider is skipped for `cooldown` (1h by default), noted in
  `~/.local/state/jev/state.json`; its first success clears it.
- Alert: `alert.command`, `{text}` replaced, once per outage and kind across
  all processes (`alerts.json`, locked). A success of the provider ends its
  outages (credit and key): the next refusal alerts again. A failed alert is a
  warning.
- A provider with `start` is started on demand and shared between processes
  (`pool-<name>.json`); the last user stops it when a user started it.

## Configuration

```yaml
providers:
  - name: jev
    url: https://api.typesafe.ai/v1/systemone
    model: jev-latest
    key: {env: [TYPESAFE_API_KEY], command: [security, find-generic-password, -s, typesafe-api-key, -w]}
    timeout: 60s
  - name: openjev
    url: http://127.0.0.1:3000/v1/systemone
    model: openjev
    start: [~/code/aclemen1/openjev/start-openjev.sh]
    ready: 10min
cooldown: 1h
alert:
  command: [notify-me, "{text}"]
  texts: {credit: "...", key: "... {provider} {message}"}
```

`state_dir` (or `$JEV_STATE_DIR`) moves the state.

## Command

```
jev status [--format json|text]   provider asked first, cooldowns, open outages, last alert
jev reset  [--format json|text]   clear cooldowns and outages (a new refusal alerts again)
```

JSON output: `{"ok": true, "result": …}` or
`{"ok": false, "error": {"code", "kind", "message"}}`.
