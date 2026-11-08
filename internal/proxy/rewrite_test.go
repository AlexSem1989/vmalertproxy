package proxy

import (
	"encoding/json"
	"maps"
	"testing"

	"vmalertsproxy/internal/config"
	"vmalertsproxy/internal/vmalert"
)

func mustGroup(t *testing.T, data string) *vmalert.Group {
	t.Helper()
	var g vmalert.Group
	if err := json.Unmarshal([]byte(data), &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

const rewriteGroupJSON = `{"name":"node","file":"node.yml","id":"1","rules":[
{"name":"HostDown","type":"alerting","id":"2","group_id":"1","group_name":"node","file":"node.yml",
"labels":{"severity":"critical"},"alerts":[{"id":"3","rule_id":"2","group_id":"1","labels":{"alertname":"HostDown"}}]},
{"name":"up:sum","type":"recording","id":"4","group_id":"1","group_name":"node","file":"node.yml","labels":{"dc":"existing"}}]}`

func TestComposeAndParseID(t *testing.T) {
	if got := composeID("msk", "123"); got != "msk:123" {
		t.Fatalf("unexpected id %q", got)
	}
	if got := composeID("msk", ""); got != "" {
		t.Fatalf("empty ids must stay empty, got %q", got)
	}
	for s, want := range map[string][3]string{
		"msk:123":   {"msk", "123", "true"},
		"msk:1:2":   {"msk", "1:2", "true"},
		"aaa-0:123": {"aaa-0", "123", "true"},
		"123":       {"", "", "false"},
		":123":      {"", "", "false"},
		"msk:":      {"", "", "false"},
		"":          {"", "", "false"},
	} {
		site, id, ok := parseID(s)
		if want[2] == "true" && (!ok || site != want[0] || id != want[1]) {
			t.Errorf("parseID(%q) = %q, %q, %v; want %q, %q", s, site, id, ok, want[0], want[1])
		}
		if want[2] == "false" && ok {
			t.Errorf("parseID(%q) must fail", s)
		}
	}
}

func TestRewriteDefaults(t *testing.T) {
	rw := &rewriter{cfg: config.Default().Rewrite}
	g := mustGroup(t, rewriteGroupJSON)
	rw.group("msk", g)

	if g.File != "msk: node.yml" || g.Name != "node" || g.ID != "msk:1" {
		t.Fatalf("unexpected group %q %q %q", g.File, g.Name, g.ID)
	}
	r := g.Rules[0]
	if r.File != "msk: node.yml" || r.GroupName != "node" || r.ID != "msk:2" || r.GroupID != "msk:1" {
		t.Fatalf("unexpected rule %+v", r)
	}
	if r.Labels["vmalert_site"] != "msk" || r.Labels["severity"] != "critical" {
		t.Fatalf("unexpected rule labels %v", r.Labels)
	}
	a := r.Alerts[0]
	if a.ID != "msk:3" || a.RuleID != "msk:2" || a.GroupID != "msk:1" {
		t.Fatalf("unexpected alert %+v", a)
	}
	if _, ok := a.Labels["vmalert_site"]; ok {
		t.Fatalf("alert labels must not be changed by default: %v", a.Labels)
	}
}

func TestRewriteCustomTemplatesAndLabels(t *testing.T) {
	rw := &rewriter{cfg: config.Rewrite{
		File:        "{file}",
		Group:       "{site}/{group} ({file})",
		SiteLabel:   "dc",
		LabelRules:  true,
		LabelAlerts: true,
	}}
	g := mustGroup(t, rewriteGroupJSON)
	rw.group("msk", g)

	if g.File != "node.yml" || g.Name != "msk/node (node.yml)" {
		t.Fatalf("unexpected group %q %q", g.File, g.Name)
	}
	if g.Rules[0].GroupName != g.Name {
		t.Fatalf("rule group name must follow the group: %q", g.Rules[0].GroupName)
	}
	if g.Rules[0].Labels["dc"] != "msk" || g.Rules[0].Alerts[0].Labels["dc"] != "msk" {
		t.Fatal("the site label must be added to rules and alerts")
	}
	if g.Rules[1].Labels["dc"] != "existing" {
		t.Fatalf("existing labels must not be overridden: %v", g.Rules[1].Labels)
	}
}

func TestRewriteWithoutSiteLabel(t *testing.T) {
	rw := &rewriter{cfg: config.Rewrite{File: "{site}: {file}", Group: "{group}", LabelRules: true, LabelAlerts: true}}
	g := mustGroup(t, rewriteGroupJSON)
	rw.group("msk", g)
	if len(g.Rules[0].Labels) != 1 || len(g.Rules[0].Alerts[0].Labels) != 1 {
		t.Fatalf("labels must not be changed when site_label is empty: %v", g.Rules[0].Labels)
	}
}

func TestRewriteStandaloneRule(t *testing.T) {
	rw := &rewriter{cfg: config.Rewrite{File: "{site}: {file}", Group: "{site}/{group}", SiteLabel: "vmalert_site", LabelRules: true}}
	var r vmalert.Rule
	if err := json.Unmarshal([]byte(`{"name":"HostDown","id":"2","group_id":"1","group_name":"node","file":"node.yml"}`), &r); err != nil {
		t.Fatal(err)
	}
	rw.rule("spb", &r)
	if r.File != "spb: node.yml" || r.GroupName != "spb/node" || r.ID != "spb:2" || r.GroupID != "spb:1" {
		t.Fatalf("a rule without its group must be rewritten from its own fields: %+v", r)
	}
}

func TestWithLabelDoesNotModifyInput(t *testing.T) {
	// Labels are shared with cached objects, so they must be copied before changes.
	in := map[string]string{"a": "b"}
	orig := maps.Clone(in)
	out := withLabel(in, "vmalert_site", "msk")
	if !maps.Equal(in, orig) {
		t.Fatalf("input labels must not be modified: %v", in)
	}
	if out["vmalert_site"] != "msk" || out["a"] != "b" {
		t.Fatalf("unexpected labels %v", out)
	}
	if got := withLabel(nil, "vmalert_site", "msk"); got["vmalert_site"] != "msk" {
		t.Fatalf("the label must be added to empty labels: %v", got)
	}
}
