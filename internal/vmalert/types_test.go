package vmalert

import (
	"encoding/json"
	"testing"
)

func TestGroupRoundTripKeepsUnknownFields(t *testing.T) {
	const data = `{"name":"node","file":"node.yml","id":"1","interval":30,"concurrency":2,"eval_offset":5,
"rules":[{"name":"HostDown","type":"alerting","state":"firing","health":"ok","id":"11","keep_firing_for":60,
"alerts":[{"id":"111","rule_id":"11","group_id":"1","labels":{"instance":"a"},"value":"1.5","restored":true}]}]}`

	var g Group
	if err := json.Unmarshal([]byte(data), &g); err != nil {
		t.Fatal(err)
	}
	g.File = "msk: node.yml"
	g.Rules[0].Alerts[0].ID = "msk:111"
	out, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["file"] != "msk: node.yml" || got["concurrency"] != 2.0 || got["eval_offset"] != 5.0 {
		t.Fatalf("unexpected group: %s", out)
	}
	rule := got["rules"].([]any)[0].(map[string]any)
	if rule["keep_firing_for"] != 60.0 {
		t.Fatalf("unknown rule fields must be kept: %s", out)
	}
	alert := rule["alerts"].([]any)[0].(map[string]any)
	if alert["id"] != "msk:111" || alert["value"] != "1.5" || alert["restored"] != true {
		t.Fatalf("unexpected alert: %s", out)
	}
}

func TestMarshalDoesNotAddMissingFields(t *testing.T) {
	// Prometheus rules API has no vmalert-specific fields.
	const data = `{"name":"node","file":"node.yml","rules":[{"name":"up:sum","type":"recording","health":"ok"}]}`
	var g Group
	if err := json.Unmarshal([]byte(data), &g); err != nil {
		t.Fatal(err)
	}
	g.ID = "msk:1"
	g.Rules[0].GroupID = "msk:1"
	out, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"file":"node.yml","name":"node","rules":[{"health":"ok","name":"up:sum","type":"recording"}]}`
	if string(out) != want {
		t.Fatalf("unexpected output\ngot:  %s\nwant: %s", out, want)
	}
}

func TestRuleWithoutAlertsOmitsAlerts(t *testing.T) {
	var r Rule
	if err := json.Unmarshal([]byte(`{"name":"a","alerts":[{"id":"1"}]}`), &r); err != nil {
		t.Fatal(err)
	}
	r.Alerts = nil
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"name":"a"}` {
		t.Fatalf("unexpected output %s", out)
	}
}

func TestGroupWithoutRulesMarshalsEmptyList(t *testing.T) {
	// vmalert never returns null rules: https://github.com/VictoriaMetrics/VictoriaMetrics/issues/4221
	out, err := json.Marshal(Group{Name: "a", File: "a.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"file":"a.yml","name":"a","rules":[]}` {
		t.Fatalf("unexpected output %s", out)
	}
}

func TestAlertLabels(t *testing.T) {
	var a Alert
	if err := json.Unmarshal([]byte(`{"id":"1","rule_id":"2","group_id":"3","value":"1"}`), &a); err != nil {
		t.Fatal(err)
	}
	a.ID, a.Labels = "msk:1", map[string]string{"dc": "msk"}
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"group_id":"3","id":"msk:1","labels":{"dc":"msk"},"rule_id":"2","value":"1"}`
	if string(out) != want {
		t.Fatalf("unexpected output\ngot:  %s\nwant: %s", out, want)
	}
}

func TestNotifierRoundTrip(t *testing.T) {
	const data = `{"kind":"static","targets":[{"address":"http://am:9093","labels":{"a":"b"},"lastError":""},{"address":"http://am2:9093"}]}`
	var n Notifier
	if err := json.Unmarshal([]byte(data), &n); err != nil {
		t.Fatal(err)
	}
	n.Targets[1].Labels = map[string]string{"dc": "msk"}
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"kind":"static","targets":[{"address":"http://am:9093","labels":{"a":"b"},"lastError":""},` +
		`{"address":"http://am2:9093","labels":{"dc":"msk"}}]}`
	if string(out) != want {
		t.Fatalf("unexpected output\ngot:  %s\nwant: %s", out, want)
	}

	var empty Notifier
	if err := json.Unmarshal([]byte(`{"kind":"blackhole"}`), &empty); err != nil {
		t.Fatal(err)
	}
	if out, _ := json.Marshal(empty); string(out) != `{"kind":"blackhole"}` {
		t.Fatalf("missing targets must not be added: %s", out)
	}
}

func TestUnmarshalErrors(t *testing.T) {
	for name, data := range map[string]string{
		"not an object":           `[1]`,
		"rules of wrong type":     `{"name":"a","rules":"r"}`,
		"rule name of wrong type": `{"name":"a","rules":[{"name":1}]}`,
		"alerts of wrong type":    `{"name":"a","rules":[{"name":"r","alerts":{}}]}`,
	} {
		var g Group
		if err := json.Unmarshal([]byte(data), &g); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	var resp Response[NotifiersData]
	if err := json.Unmarshal([]byte(`{"status":"success","data":{"notifiers":[{"targets":"x"}]}}`), &resp); err == nil {
		t.Error("expected an error for targets of wrong type")
	}
}

func TestExtendState(t *testing.T) {
	zero := 0
	f := func(r Rule, want string) {
		t.Helper()
		r.ExtendState()
		if r.State != want {
			t.Errorf("unexpected state %q, want %q", r.State, want)
		}
	}
	f(Rule{State: "firing", Health: "ok", Alerts: []*Alert{{}}}, "firing")
	f(Rule{State: "inactive", Health: "err"}, "unhealthy")
	f(Rule{State: "", Health: "ok", LastSeriesFetched: &zero}, "nomatch")
	f(Rule{State: "", Health: "ok", LastSamples: 1}, "ok")
	f(Rule{State: "inactive", Health: "ok"}, "inactive")
}
