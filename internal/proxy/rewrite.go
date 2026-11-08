package proxy

import (
	"maps"
	"strings"

	"vmalertsproxy/internal/config"
	"vmalertsproxy/internal/vmalert"
)

// idSeparator separates the backend name from the original vmalert id.
//
// vmalert ids are hashes of the group and rule configs, so identical rules
// of different backends have identical ids.
const idSeparator = ":"

func composeID(site, id string) string {
	if id == "" {
		return ""
	}
	return site + idSeparator + id
}

// parseID splits the id made by composeID.
func parseID(s string) (site, id string, ok bool) {
	site, id, ok = strings.Cut(s, idSeparator)
	return site, id, ok && site != "" && id != ""
}

// rewriter makes groups, rules and alerts of different backends distinguishable after merging.
//
// Rules with the same expression on different backends are different rules,
// so they must not be merged by Grafana into a single rule.
type rewriter struct {
	cfg config.Rewrite
}

func (rw *rewriter) render(tpl, site, file, group string) string {
	return strings.NewReplacer("{site}", site, "{file}", file, "{group}", group).Replace(tpl)
}

func (rw *rewriter) group(site string, g *vmalert.Group) {
	file := rw.render(rw.cfg.File, site, g.File, g.Name)
	name := rw.render(rw.cfg.Group, site, g.File, g.Name)
	g.File, g.Name, g.ID = file, name, composeID(site, g.ID)
	for _, r := range g.Rules {
		rw.ruleInGroup(site, r, file, name)
	}
}

// rule rewrites a rule returned without its group, e.g. by /api/v1/rule.
func (rw *rewriter) rule(site string, r *vmalert.Rule) {
	file := rw.render(rw.cfg.File, site, r.File, r.GroupName)
	name := rw.render(rw.cfg.Group, site, r.File, r.GroupName)
	rw.ruleInGroup(site, r, file, name)
}

func (rw *rewriter) ruleInGroup(site string, r *vmalert.Rule, file, group string) {
	r.File, r.GroupName = file, group
	r.ID, r.GroupID = composeID(site, r.ID), composeID(site, r.GroupID)
	if rw.cfg.LabelRules {
		r.Labels = withLabel(r.Labels, rw.cfg.SiteLabel, site)
	}
	for _, a := range r.Alerts {
		rw.alert(site, a)
	}
}

func (rw *rewriter) alert(site string, a *vmalert.Alert) {
	a.ID, a.RuleID, a.GroupID = composeID(site, a.ID), composeID(site, a.RuleID), composeID(site, a.GroupID)
	if rw.cfg.LabelAlerts {
		a.Labels = withLabel(a.Labels, rw.cfg.SiteLabel, site)
	}
}

func (rw *rewriter) notifier(site string, n *vmalert.Notifier) {
	for _, t := range n.Targets {
		t.Labels = withLabel(t.Labels, rw.cfg.SiteLabel, site)
	}
}

// withLabel returns labels with name=value added. Existing labels are never overridden.
func withLabel(labels map[string]string, name, value string) map[string]string {
	if name == "" {
		return labels
	}
	if _, ok := labels[name]; ok {
		return labels
	}
	out := make(map[string]string, len(labels)+1)
	maps.Copy(out, labels)
	out[name] = value
	return out
}
