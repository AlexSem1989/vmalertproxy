package proxy

import (
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/VictoriaMetrics/metricsql"

	"vmalertsproxy/internal/vmalert"
)

// Filters are applied by the proxy to the merged groups instead of being forwarded to backends,
// since group files and names are rewritten and pagination must cover groups of all backends.
// The semantics follow vmalert, see app/vmalert/web.go in VictoriaMetrics.

// groupsFilter selects groups by the rule_group[], file[] and datasource_type params.
type groupsFilter struct {
	names  []string
	files  []string
	dsType string
}

func newGroupsFilter(form url.Values) *groupsFilter {
	return &groupsFilter{
		names:  listParam(form, "rule_group"),
		files:  listParam(form, "file"),
		dsType: form.Get("datasource_type"),
	}
}

// listParam returns values of both the Prometheus-style "name[]" and the Mimir-style "name" params.
// Grafana sends both of them with the same values.
func listParam(form url.Values, name string) []string {
	return append(slices.Clone(form[name+"[]"]), form[name]...)
}

func (gf *groupsFilter) matches(g *vmalert.Group) bool {
	if len(gf.names) > 0 && !slices.Contains(gf.names, g.Name) {
		return false
	}
	if len(gf.files) > 0 && !slices.Contains(gf.files, g.File) {
		return false
	}
	// Groups of Prometheus-compatible backends have no type.
	return gf.dsType == "" || gf.dsType == cmp.Or(g.Type, "prometheus")
}

var ruleTypes = map[string]string{
	"alert":  vmalert.TypeAlerting,
	"record": vmalert.TypeRecording,
}

// ruleStates maps the accepted values of the state param to vmalert rule states.
// "noData", "normal" and "error" are Grafana aliases.
var ruleStates = map[string]string{
	"ok":         "ok",
	"nomatch":    "nomatch",
	"inactive":   "inactive",
	"firing":     "firing",
	"pending":    "pending",
	"unhealthy":  "unhealthy",
	"recovering": "recovering",
	"noData":     "nomatch",
	"normal":     "inactive",
	"error":      "unhealthy",
}

// rulesFilter selects and paginates groups and rules for /api/v1/rules.
type rulesFilter struct {
	groups         *groupsFilter
	names          []string
	ruleType       string
	states         []string
	match          labelMatchers
	search         string
	excludeAlerts  bool
	extendedStates bool
	groupLimit     int
	pageNum        int
	nextToken      *groupKey
}

func newRulesFilter(form url.Values) (*rulesFilter, error) {
	rf := &rulesFilter{
		groups:         newGroupsFilter(form),
		names:          listParam(form, "rule_name"),
		search:         strings.ToLower(form.Get("search")),
		excludeAlerts:  boolParam(form, "exclude_alerts"),
		extendedStates: boolParam(form, "extended_states"),
	}
	if v := form.Get("type"); v != "" {
		t, ok := ruleTypes[v]
		if !ok {
			return nil, fmt.Errorf(`invalid parameter "type": not supported value %q`, v)
		}
		rf.ruleType = t
	}
	var err error
	if rf.match, err = parseMatchers(form["match[]"]); err != nil {
		return nil, err
	}
	states := form["state"]
	if len(states) == 0 {
		states = form["filter"]
	}
	for _, s := range states {
		for v := range strings.SplitSeq(s, ",") {
			if v == "" {
				continue
			}
			state, ok := ruleStates[v]
			if !ok {
				return nil, fmt.Errorf(`invalid parameter "state": contains not supported value %q`, v)
			}
			rf.states = append(rf.states, state)
		}
	}
	if rf.groupLimit, err = positiveIntParam(form, "group_limit"); err != nil {
		return nil, err
	}
	if rf.pageNum, err = positiveIntParam(form, "page_num"); err != nil {
		return nil, err
	}
	if rf.pageNum > 0 && rf.groupLimit == 0 {
		return nil, errors.New(`"group_limit" needs to be present in order to paginate over the groups`)
	}
	if token := form.Get("group_next_token"); token != "" {
		key, err := parseGroupToken(token)
		if err != nil {
			return nil, err
		}
		rf.nextToken = &key
	}
	return rf, nil
}

// boolParam parses the param in the same way as vmalert does.
func boolParam(form url.Values, name string) bool {
	switch strings.ToLower(form.Get(name)) {
	case "", "0", "f", "false", "no":
		return false
	default:
		return true
	}
}

func positiveIntParam(form url.Values, name string) (int, error) {
	s := form.Get(name)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%q is expected to be a positive number, found %q", name, s)
	}
	return v, nil
}

// rulesPage is a page of filtered groups.
type rulesPage struct {
	groups      []*vmalert.Group
	nextToken   string
	page        int
	totalPages  int
	totalGroups int
	totalRules  int
}

// apply filters and paginates groups sorted by sortGroups.
//
// Two pagination styles are supported:
//   - group_limit + page_num with page counters in the response, as in vmalert (used by vmui);
//   - group_limit + group_next_token with groupNextToken in the response, as in Prometheus 3 (used by Grafana 12+).
func (rf *rulesFilter) apply(groups []*vmalert.Group) (*rulesPage, error) {
	res := &rulesPage{groups: make([]*vmalert.Group, 0)}
	skip := 0
	if rf.pageNum > 0 {
		skip = (rf.pageNum - 1) * rf.groupLimit
	}
	for _, g := range groups {
		if !rf.groups.matches(g) {
			continue
		}
		if rf.pageNum == 0 && rf.nextToken != nil && rf.nextToken.compare(g) > 0 {
			continue
		}
		fg, ok := rf.filterGroup(g)
		if !ok {
			continue
		}
		if rf.pageNum > 0 {
			res.totalGroups++
			res.totalRules += len(fg.Rules)
			if skip > 0 {
				skip--
				continue
			}
			if len(res.groups) < rf.groupLimit {
				res.groups = append(res.groups, fg)
			}
			continue
		}
		if rf.groupLimit > 0 && len(res.groups) == rf.groupLimit {
			res.nextToken = keyOf(g).token()
			break
		}
		res.groups = append(res.groups, fg)
	}
	if rf.pageNum > 0 {
		res.page = rf.pageNum
		res.totalPages = max((res.totalGroups+rf.groupLimit-1)/rf.groupLimit, 1)
		if res.page > res.totalPages {
			return nil, fmt.Errorf("page_num=%d exceeds total amount of pages in result=%d", res.page, res.totalPages)
		}
	}
	return res, nil
}

// filterGroup returns a copy of g with the matching rules only.
// Groups and rules are shared between requests, so they must not be modified.
func (rf *rulesFilter) filterGroup(g *vmalert.Group) (*vmalert.Group, bool) {
	groupFound := rf.search == "" ||
		strings.Contains(strings.ToLower(g.Name), rf.search) ||
		strings.Contains(strings.ToLower(g.File), rf.search)
	rules := make([]*vmalert.Rule, 0, len(g.Rules))
	states := make(map[string]int)
	for _, r := range g.Rules {
		if !groupFound && !strings.Contains(strings.ToLower(r.Name), rf.search) {
			continue
		}
		rc := *r
		if rf.extendedStates {
			rc.ExtendState()
		}
		if !rf.matchesRule(&rc) {
			continue
		}
		if rf.excludeAlerts {
			rc.Alerts = nil
		}
		states[rc.State]++
		rules = append(rules, &rc)
	}
	// vmalert keeps groups without rules in the response.
	if len(g.Rules) > 0 && len(rules) == 0 {
		return nil, false
	}
	gc := *g
	gc.Rules = rules
	gc.States = states
	return &gc, true
}

func (rf *rulesFilter) matchesRule(r *vmalert.Rule) bool {
	if rf.ruleType != "" && rf.ruleType != r.Type {
		return false
	}
	if len(rf.names) > 0 && !slices.Contains(rf.names, r.Name) {
		return false
	}
	if !rf.match.matches(r.Labels) {
		return false
	}
	return len(rf.states) == 0 || slices.Contains(rf.states, r.State)
}

// alertsFilter selects alerts for /api/v1/alerts.
type alertsFilter struct {
	groups *groupsFilter
	match  labelMatchers
}

func newAlertsFilter(form url.Values) (*alertsFilter, error) {
	match, err := parseMatchers(form["match[]"])
	if err != nil {
		return nil, err
	}
	return &alertsFilter{groups: newGroupsFilter(form), match: match}, nil
}

func (af *alertsFilter) apply(groups []*vmalert.Group) []*vmalert.Alert {
	alerts := make([]*vmalert.Alert, 0)
	for _, g := range groups {
		if !af.groups.matches(g) {
			continue
		}
		for _, r := range g.Rules {
			if r.Type != vmalert.TypeAlerting {
				continue
			}
			for _, a := range r.Alerts {
				if af.match.matches(a.Labels) {
					alerts = append(alerts, a)
				}
			}
		}
	}
	slices.SortStableFunc(alerts, func(a, b *vmalert.Alert) int {
		return strings.Compare(a.ID, b.ID)
	})
	return alerts
}

// groupKey identifies a group in the merged list.
type groupKey struct {
	file string
	name string
}

func keyOf(g *vmalert.Group) groupKey {
	return groupKey{file: g.File, name: g.Name}
}

func (k groupKey) compare(g *vmalert.Group) int {
	return cmp.Or(cmp.Compare(k.file, g.File), cmp.Compare(k.name, g.Name))
}

// token encodes the key of the first group of the next page.
//
// Since groups are sorted by key, the next page starts from the first group
// that is not less than the key, even if that group has been removed meanwhile.
func (k groupKey) token() string {
	return base64.RawURLEncoding.EncodeToString([]byte(k.file + "\x00" + k.name))
}

func parseGroupToken(token string) (groupKey, error) {
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return groupKey{}, fmt.Errorf("invalid group_next_token %q", token)
	}
	file, name, ok := strings.Cut(string(data), "\x00")
	if !ok {
		return groupKey{}, fmt.Errorf("invalid group_next_token %q", token)
	}
	return groupKey{file: file, name: name}, nil
}

// sortGroups sorts groups by file and name, so groups of the same Grafana namespace are adjacent.
// The sort is stable, so groups with equal keys keep the order of backends.
func sortGroups(groups []*vmalert.Group) {
	slices.SortStableFunc(groups, func(a, b *vmalert.Group) int {
		return keyOf(a).compare(b)
	})
}

// labelMatchers holds series selectors from match[] params.
// Labels match if they match all the filters of at least one selector.
type labelMatchers [][]labelMatcher

type labelMatcher struct {
	label    string
	value    string
	negative bool
	re       *regexp.Regexp
}

func parseMatchers(selectors []string) (labelMatchers, error) {
	var lms labelMatchers
	for _, s := range selectors {
		expr, err := metricsql.Parse(s)
		if err != nil {
			return nil, fmt.Errorf(`invalid parameter "match[]": failed to parse %q: %w`, s, err)
		}
		me, ok := expr.(*metricsql.MetricExpr)
		if !ok {
			return nil, fmt.Errorf(`invalid parameter "match[]": expecting metricSelector; got %q`, expr.AppendString(nil))
		}
		if len(me.LabelFilterss) == 0 {
			return nil, errors.New(`invalid parameter "match[]": labelFilterss cannot be empty`)
		}
		for _, lfs := range me.LabelFilterss {
			ms := make([]labelMatcher, 0, len(lfs))
			for _, lf := range lfs {
				m := labelMatcher{label: lf.Label, value: lf.Value, negative: lf.IsNegative}
				if lf.IsRegexp {
					if m.re, err = metricsql.CompileRegexpAnchored(lf.Value); err != nil {
						return nil, fmt.Errorf(`invalid parameter "match[]": invalid regexp %q: %w`, lf.Value, err)
					}
				}
				ms = append(ms, m)
			}
			lms = append(lms, ms)
		}
	}
	return lms, nil
}

func (lms labelMatchers) matches(labels map[string]string) bool {
	if len(lms) == 0 {
		return true
	}
	return slices.ContainsFunc(lms, func(ms []labelMatcher) bool {
		for _, m := range ms {
			if !m.matches(labels[m.label]) {
				return false
			}
		}
		return true
	})
}

func (m labelMatcher) matches(v string) bool {
	ok := v == m.value
	if m.re != nil {
		ok = m.re.MatchString(v)
	}
	return ok != m.negative
}
