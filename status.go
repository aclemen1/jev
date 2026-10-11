package jev

import "time"

// ProviderStatus is one provider as `jev status` shows it.
type ProviderStatus struct {
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Model    string    `json:"model,omitempty"`
	Cooldown *Cooldown `json:"cooldown,omitempty"`
	Local    *PoolInfo `json:"local,omitempty"` // a provider with a start command
}

// Status is the state of the providers: the one asked first, the cooldowns, the last alert.
type Status struct {
	Config    string            `json:"config"`
	Active    string            `json:"active"` // the first provider not in cooldown; "" when all are
	Providers []ProviderStatus  `json:"providers"`
	Cooldown  string            `json:"cooldown"`
	LastAlert *SentAlert        `json:"last_alert,omitempty"`
	Outages   map[string]Outage `json:"outages"` // kinds alerted and not over: no new alert until the provider answers
}

// Status reads the state without asking any provider (a local one gets a health check).
func (c *Config) Status(now time.Time) Status {
	st := c.ReadState()
	al := c.ReadAlerts()
	out := Status{Config: c.path, Cooldown: time.Duration(c.Cooldown).String(), LastAlert: al.Last, Outages: al.Open}
	for _, p := range c.Providers {
		ps := ProviderStatus{Name: p.Name, URL: p.URL, Model: p.Model}
		if cd, ok := st.Cooldowns[p.Name]; ok && now.Before(cd.Until) {
			cd := cd
			ps.Cooldown = &cd
		} else if out.Active == "" {
			out.Active = p.Name
		}
		if len(p.Start) > 0 {
			info := c.poolInfo(p)
			ps.Local = &info
		}
		out.Providers = append(out.Providers, ps)
	}
	return out
}
