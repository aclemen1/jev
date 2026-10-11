package jev

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Kinds of failure.
const (
	KindCredit      = "credit"      // credit exhausted: cooldown and alert
	KindKey         = "key"         // key refused: cooldown and alert
	KindUnavailable = "unavailable" // 5xx, plain 429, connection, timeout, unreadable answer: fall back, no alert
)

var creditRe = regexp.MustCompile(`(?i)credit|balance|quota|insufficient|payment`)

// Classify sorts a failed HTTP answer by its status and the message of its body.
func Classify(status int, body []byte) (kind, message string) {
	message = errorMessage(body)
	credit := creditRe.MatchString(message)
	switch {
	case status == 402:
		return KindCredit, message
	case status == 401:
		return KindKey, message
	case status == 403 && credit, status == 429 && credit:
		return KindCredit, message
	case status == 403:
		return KindKey, message
	}
	return KindUnavailable, message
}

// errorMessage reads detail.error_type, detail.message, detail (a string),
// error.type and error.message (or error, a string), joined by ": ".
func errorMessage(body []byte) string {
	var b struct {
		Detail json.RawMessage `json:"detail"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &b) != nil {
		return strings.TrimSpace(truncate(string(body), 300))
	}
	var parts []string
	for _, raw := range []json.RawMessage{b.Detail, b.Error} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			parts = append(parts, s)
			continue
		}
		var o struct {
			ErrorType string `json:"error_type"`
			Type      string `json:"type"`
			Code      any    `json:"code"`
			Message   string `json:"message"`
		}
		if json.Unmarshal(raw, &o) == nil {
			for _, x := range []string{o.ErrorType, o.Type, o.Message} {
				if x != "" {
					parts = append(parts, x)
				}
			}
			continue
		}
		parts = append(parts, truncate(string(raw), 300)) // a list of validation errors, …
	}
	return strings.Join(parts, ": ")
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
