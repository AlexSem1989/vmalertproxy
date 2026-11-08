// Package vmalert describes objects of the vmalert HTTP API.
//
// Only the fields used by the proxy are decoded. All the other fields are kept
// as raw JSON and written back unchanged, so new vmalert fields pass through
// the proxy without code changes.
package vmalert

import (
	"encoding/json"
	"fmt"
	"maps"
)

// Rule types returned in the "type" field of a rule.
const (
	TypeAlerting  = "alerting"
	TypeRecording = "recording"
)

// Response is the envelope of vmalert list APIs.
type Response[T any] struct {
	Status string `json:"status"`
	Data   T      `json:"data"`
	Error  string `json:"error"`
}

// RulesData is the data of /api/v1/rules response.
type RulesData struct {
	Groups []*Group `json:"groups"`
}

// NotifiersData is the data of /api/v1/notifiers response.
type NotifiersData struct {
	Notifiers []*Notifier `json:"notifiers"`
}

// Group is a rules group returned by /api/v1/rules and /api/v1/group.
type Group struct {
	Name string
	File string
	ID   string
	// Type is the datasource type of the group: prometheus, graphite, vlogs, etc.
	Type  string
	Rules []*Rule
	// States holds the number of rules per state.
	States map[string]int

	raw rawObject
}

// UnmarshalJSON implements json.Unmarshaler.
func (g *Group) UnmarshalJSON(data []byte) error {
	return g.raw.unmarshal(data, map[string]any{
		"name":   &g.Name,
		"file":   &g.File,
		"id":     &g.ID,
		"type":   &g.Type,
		"rules":  &g.Rules,
		"states": &g.States,
	})
}

// MarshalJSON implements json.Marshaler.
func (g Group) MarshalJSON() ([]byte, error) {
	p := newPatch(g.raw)
	p.set("name", g.Name)
	p.set("file", g.File)
	p.update("id", g.ID)
	rules := g.Rules
	if rules == nil {
		rules = []*Rule{}
	}
	p.set("rules", rules)
	p.update("states", g.States)
	return p.marshal()
}

// Rule is an alerting or recording rule.
type Rule struct {
	Name              string
	Type              string
	State             string
	Health            string
	ID                string
	GroupID           string
	GroupName         string
	File              string
	Labels            map[string]string
	Alerts            []*Alert
	LastSamples       int
	LastSeriesFetched *int

	raw rawObject
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *Rule) UnmarshalJSON(data []byte) error {
	return r.raw.unmarshal(data, map[string]any{
		"name":              &r.Name,
		"type":              &r.Type,
		"state":             &r.State,
		"health":            &r.Health,
		"id":                &r.ID,
		"group_id":          &r.GroupID,
		"group_name":        &r.GroupName,
		"file":              &r.File,
		"labels":            &r.Labels,
		"alerts":            &r.Alerts,
		"lastSamples":       &r.LastSamples,
		"lastSeriesFetched": &r.LastSeriesFetched,
	})
}

// MarshalJSON implements json.Marshaler.
func (r Rule) MarshalJSON() ([]byte, error) {
	p := newPatch(r.raw)
	p.update("id", r.ID)
	p.update("group_id", r.GroupID)
	p.update("group_name", r.GroupName)
	p.update("file", r.File)
	if r.State != "" {
		p.set("state", r.State)
	}
	if len(r.Labels) > 0 {
		p.set("labels", r.Labels)
	}
	// vmalert omits alerts of recording rules and of rules filtered with exclude_alerts.
	if r.Alerts != nil {
		p.set("alerts", r.Alerts)
	} else {
		p.del("alerts")
	}
	return p.marshal()
}

// ExtendState sets the "ok", "unhealthy" and "nomatch" states
// in the same way as vmalert does for the extended_states request param.
func (r *Rule) ExtendState() {
	if len(r.Alerts) > 0 {
		return
	}
	if r.State == "" {
		r.State = "ok"
	}
	if r.Health != "ok" {
		r.State = "unhealthy"
	} else if r.LastSamples == 0 && r.LastSeriesFetched != nil && *r.LastSeriesFetched == 0 {
		r.State = "nomatch"
	}
}

// Alert is an active alert of an alerting rule.
type Alert struct {
	ID      string
	RuleID  string
	GroupID string
	Labels  map[string]string

	raw rawObject
}

// UnmarshalJSON implements json.Unmarshaler.
func (a *Alert) UnmarshalJSON(data []byte) error {
	return a.raw.unmarshal(data, map[string]any{
		"id":       &a.ID,
		"rule_id":  &a.RuleID,
		"group_id": &a.GroupID,
		"labels":   &a.Labels,
	})
}

// MarshalJSON implements json.Marshaler.
func (a Alert) MarshalJSON() ([]byte, error) {
	p := newPatch(a.raw)
	p.update("id", a.ID)
	p.update("rule_id", a.RuleID)
	p.update("group_id", a.GroupID)
	if len(a.Labels) > 0 {
		p.set("labels", a.Labels)
	}
	return p.marshal()
}

// Notifier is a set of notification targets of the same kind.
type Notifier struct {
	Targets []*Target

	raw rawObject
}

// UnmarshalJSON implements json.Unmarshaler.
func (n *Notifier) UnmarshalJSON(data []byte) error {
	return n.raw.unmarshal(data, map[string]any{"targets": &n.Targets})
}

// MarshalJSON implements json.Marshaler.
func (n Notifier) MarshalJSON() ([]byte, error) {
	p := newPatch(n.raw)
	if n.Targets != nil {
		p.set("targets", n.Targets)
	}
	return p.marshal()
}

// Target is a notification target, e.g. an Alertmanager instance.
type Target struct {
	Labels map[string]string

	raw rawObject
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Target) UnmarshalJSON(data []byte) error {
	return t.raw.unmarshal(data, map[string]any{"labels": &t.Labels})
}

// MarshalJSON implements json.Marshaler.
func (t Target) MarshalJSON() ([]byte, error) {
	p := newPatch(t.raw)
	if len(t.Labels) > 0 {
		p.set("labels", t.Labels)
	}
	return p.marshal()
}

// rawObject holds all the fields of a JSON object.
type rawObject map[string]json.RawMessage

// unmarshal stores all the fields of the JSON object in o and decodes the given fields into their destinations.
func (o *rawObject) unmarshal(data []byte, fields map[string]any) error {
	if err := json.Unmarshal(data, o); err != nil {
		return err
	}
	for key, dst := range fields {
		v, ok := (*o)[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(v, dst); err != nil {
			return fmt.Errorf("cannot parse %q: %w", key, err)
		}
	}
	return nil
}

// patch writes fields over a copy of rawObject, so the original object can be shared between goroutines.
type patch struct {
	out rawObject
	err error
}

func newPatch(raw rawObject) *patch {
	out := maps.Clone(raw)
	if out == nil {
		out = rawObject{}
	}
	return &patch{out: out}
}

func (p *patch) set(key string, v any) {
	if p.err != nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		p.err = fmt.Errorf("cannot marshal %q: %w", key, err)
		return
	}
	p.out[key] = data
}

// update sets the field only if the original object has it,
// so the proxy doesn't add fields the backend doesn't return.
func (p *patch) update(key string, v any) {
	if _, ok := p.out[key]; ok {
		p.set(key, v)
	}
}

func (p *patch) del(key string) {
	delete(p.out, key)
}

func (p *patch) marshal() ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	return json.Marshal(p.out)
}
