package proxy

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"vmalertsproxy/internal/vmalert"
)

func mustRulesFilter(t *testing.T, query string) *rulesFilter {
	t.Helper()
	form, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := newRulesFilter(form)
	if err != nil {
		t.Fatalf("%s: %s", query, err)
	}
	return rf
}

// testGroups returns sorted groups a.yml/a, b.yml/b, c.yml/c, d.yml/d with a single firing rule each.
func testGroups(t *testing.T) []*vmalert.Group {
	t.Helper()
	var groups []*vmalert.Group
	for _, name := range []string{"d", "b", "a", "c"} {
		groups = append(groups, mustGroup(t, `{"name":"`+name+`","file":"`+name+`.yml","rules":[
{"name":"r","type":"alerting","state":"firing","health":"ok","alerts":[{"id":"`+name+`"}]}]}`))
	}
	sortGroups(groups)
	return groups
}

func names(groups []*vmalert.Group) []string {
	var s []string
	for _, g := range groups {
		s = append(s, g.Name)
	}
	return s
}

func TestNewRulesFilterErrors(t *testing.T) {
	for _, query := range []string{
		"type=alerting",
		"state=resolved",
		"filter=resolved",
		"state=firing,bad",
		"match[]=sum(up)",
		"match[]={a=~\"(\"}",
		"match[]={",
		"group_limit=0",
		"group_limit=abc",
		"page_num=1",
		"group_limit=1&page_num=0",
		"group_next_token=!!!",
		"group_next_token=bm8tc2VwYXJhdG9y",
	} {
		form, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := newRulesFilter(form); err == nil {
			t.Errorf("%s: expected an error", query)
		}
	}
}

func TestListParamMergesPrometheusAndMimirForms(t *testing.T) {
	form := url.Values{"file[]": {"a.yml"}, "file": {"b.yml"}}
	if got := listParam(form, "file"); !slices.Equal(got, []string{"a.yml", "b.yml"}) {
		t.Fatalf("unexpected values %q", got)
	}
}

func TestBoolParam(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "f": false, "false": false, "No": false, "1": true, "true": true, "yes": true} {
		if got := boolParam(url.Values{"p": {v}}, "p"); got != want {
			t.Errorf("boolParam(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestGroupsFilterDatasourceType(t *testing.T) {
	untyped := mustGroup(t, `{"name":"a","file":"a.yml","rules":[]}`)
	graphite := mustGroup(t, `{"name":"b","file":"b.yml","type":"graphite","rules":[]}`)
	gf := newGroupsFilter(url.Values{"datasource_type": {"prometheus"}})
	if !gf.matches(untyped) || gf.matches(graphite) {
		t.Fatal("groups without type must be treated as Prometheus groups")
	}
}

func TestLabelMatchers(t *testing.T) {
	labels := map[string]string{"severity": "critical", "team": "infra"}
	f := func(selector string, want bool) {
		t.Helper()
		lms, err := parseMatchers([]string{selector})
		if err != nil {
			t.Fatalf("%s: %s", selector, err)
		}
		if got := lms.matches(labels); got != want {
			t.Errorf("%s: got %v, want %v", selector, got, want)
		}
	}
	f(`{severity="critical"}`, true)
	f(`{severity="warning"}`, false)
	f(`{severity!="warning"}`, true)
	f(`{severity=~"crit.*"}`, true)
	// Regexps are anchored like in Prometheus.
	f(`{severity=~"crit"}`, false)
	f(`{severity!~"warn.*"}`, true)
	f(`{severity="critical",team="db"}`, false)
	f(`{severity="warning" or team="infra"}`, true)
	// A missing label equals an empty value.
	f(`{team="infra",owner=""}`, true)
	f(`{team="infra",owner!=""}`, false)

	lms, err := parseMatchers([]string{`{team="db"}`, `{team="infra"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !lms.matches(labels) {
		t.Fatal("labels must match at least one of match[] selectors")
	}
	if !labelMatchers(nil).matches(labels) {
		t.Fatal("empty matchers must match any labels")
	}
}

func TestTokenPaginationContinuesAfterRemovedGroup(t *testing.T) {
	groups := testGroups(t)
	page := mustApply(t, mustRulesFilter(t, "group_limit=2"), groups)
	if got := names(page.groups); !slices.Equal(got, []string{"a", "b"}) || page.nextToken == "" {
		t.Fatalf("unexpected first page %q, token %q", got, page.nextToken)
	}
	// The group the token points to has been removed before the next page is requested.
	withoutC := slices.DeleteFunc(slices.Clone(groups), func(g *vmalert.Group) bool { return g.Name == "c" })
	page = mustApply(t, mustRulesFilter(t, "group_limit=2&group_next_token="+page.nextToken), withoutC)
	if got := names(page.groups); !slices.Equal(got, []string{"d"}) || page.nextToken != "" {
		t.Fatalf("the next page must start from the next existing group, got %q, token %q", got, page.nextToken)
	}
}

func TestTokenWithoutLimitReturnsRemainingGroups(t *testing.T) {
	groups := testGroups(t)
	token := keyOf(groups[2]).token()
	page := mustApply(t, mustRulesFilter(t, "group_next_token="+token), groups)
	if got := names(page.groups); !slices.Equal(got, []string{"c", "d"}) {
		t.Fatalf("unexpected groups %q", got)
	}
}

func TestPageNumIgnoresToken(t *testing.T) {
	groups := testGroups(t)
	token := keyOf(groups[3]).token()
	page := mustApply(t, mustRulesFilter(t, "group_limit=2&page_num=1&group_next_token="+token), groups)
	if got := names(page.groups); !slices.Equal(got, []string{"a", "b"}) || page.nextToken != "" {
		t.Fatalf("page_num pagination must ignore group_next_token, got %q", got)
	}
	if page.totalGroups != 4 || page.totalRules != 4 || page.totalPages != 2 {
		t.Fatalf("unexpected counters %+v", page)
	}
}

func TestPaginationOfFilteredGroups(t *testing.T) {
	groups := testGroups(t)
	// Filters are applied before pagination, so pages contain matching groups only.
	page := mustApply(t, mustRulesFilter(t, "group_limit=1&rule_group[]=b&rule_group[]=d"), groups)
	if got := names(page.groups); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("unexpected first page %q", got)
	}
	page = mustApply(t, mustRulesFilter(t, "group_limit=1&rule_group[]=b&rule_group[]=d&group_next_token="+page.nextToken), groups)
	if got := names(page.groups); !slices.Equal(got, []string{"d"}) || page.nextToken != "" {
		t.Fatalf("unexpected second page %q", got)
	}
}

func TestFilterDoesNotModifySharedGroups(t *testing.T) {
	g := mustGroup(t, `{"name":"a","file":"a.yml","states":{},"rules":[
{"name":"r1","type":"alerting","state":"firing","health":"ok","alerts":[{"id":"1"}]},
{"name":"r2","type":"recording","state":"","health":"err"}]}`)
	page := mustApply(t, mustRulesFilter(t, "exclude_alerts=true&extended_states=true"), []*vmalert.Group{g})
	fg := page.groups[0]
	if fg.Rules[0].Alerts != nil || fg.Rules[1].State != "unhealthy" {
		t.Fatalf("unexpected filtered rules %+v", fg.Rules)
	}
	if g.Rules[0].Alerts == nil || g.Rules[1].State != "" || g.States != nil && len(g.States) != 0 {
		t.Fatal("cached groups must not be modified by filtering")
	}
	if fg.States["firing"] != 1 || fg.States["unhealthy"] != 1 {
		t.Fatalf("states must be counted over filtered rules: %v", fg.States)
	}
}

func TestSearch(t *testing.T) {
	groups := testGroups(t)
	for query, want := range map[string][]string{
		"search=B.YML": {"b"},
		"search=b":     {"b"},
		"search=r":     {"a", "b", "c", "d"},
		"search=zzz":   nil,
	} {
		page := mustApply(t, mustRulesFilter(t, query), groups)
		if got := names(page.groups); !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", query, got, want)
		}
	}
}

func TestAlertsFilter(t *testing.T) {
	groups := []*vmalert.Group{
		mustGroup(t, `{"name":"a","file":"a.yml","rules":[
{"name":"r","type":"alerting","alerts":[{"id":"msk:2","labels":{"severity":"critical"}},{"id":"msk:1","labels":{"severity":"warning"}}]},
{"name":"rec","type":"recording","alerts":[{"id":"msk:0"}]}]}`),
		mustGroup(t, `{"name":"b","file":"b.yml","type":"vlogs","rules":[{"name":"r","type":"alerting","alerts":[{"id":"msk:3"}]}]}`),
	}
	f := func(query string, want ...string) {
		t.Helper()
		form, _ := url.ParseQuery(query)
		af, err := newAlertsFilter(form)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, a := range af.apply(groups) {
			ids = append(ids, a.ID)
		}
		if !slices.Equal(ids, want) {
			t.Errorf("%s: got %q, want %q", query, ids, want)
		}
	}
	// Alerts of recording rules are ignored and alerts are sorted by id.
	f("", "msk:1", "msk:2", "msk:3")
	f("datasource_type=prometheus", "msk:1", "msk:2")
	f("file[]=b.yml", "msk:3")
	f(`match[]={severity="critical"}`, "msk:2")
	if _, err := newAlertsFilter(url.Values{"match[]": {"sum(x)"}}); err == nil {
		t.Fatal("expected an error for an invalid match[]")
	}
}

func TestGroupToken(t *testing.T) {
	g := mustGroup(t, `{"name":"node","file":"msk: /etc/vmalert/rules.yml","rules":[]}`)
	key, err := parseGroupToken(keyOf(g).token())
	if err != nil {
		t.Fatal(err)
	}
	if key.compare(g) != 0 {
		t.Fatalf("token must point to the group, got %+v", key)
	}
	if strings.ContainsAny(keyOf(g).token(), "+/=") {
		t.Fatal("tokens must be url-safe")
	}
}

func mustApply(t *testing.T, rf *rulesFilter, groups []*vmalert.Group) *rulesPage {
	t.Helper()
	page, err := rf.apply(groups)
	if err != nil {
		t.Fatal(err)
	}
	return page
}
