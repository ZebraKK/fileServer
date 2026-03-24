// Package rewrite provides a URL-rewriting pipeline plugin.
// Rules are evaluated in order; the first match wins and rewrites pCtx.RewrittenPath.
package rewrite

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"fileServer/internal/observe"
	"fileServer/internal/pipeline"
)

func init() {
	pipeline.RegisterFactory("url_rewrite", func(cfg map[string]any) (pipeline.Plugin, error) {
		return fromConfig(cfg)
	})
}

type rule struct {
	re      *regexp.Regexp
	replace string
}

// Plugin rewrites request paths according to a list of regex rules.
type Plugin struct {
	rules []rule
}

func fromConfig(cfg map[string]any) (*Plugin, error) {
	rawRules, _ := cfg["rules"].([]any)
	var rules []rule
	for i, raw := range rawRules {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("url_rewrite: rule[%d]: expected map", i)
		}
		match, _ := m["match"].(string)
		replace, _ := m["replace"].(string)
		if match == "" {
			return nil, fmt.Errorf("url_rewrite: rule[%d]: 'match' is required", i)
		}
		re, err := regexp.Compile(match)
		if err != nil {
			return nil, fmt.Errorf("url_rewrite: rule[%d]: invalid regex %q: %w", i, match, err)
		}
		rules = append(rules, rule{re: re, replace: replace})
	}
	return &Plugin{rules: rules}, nil
}

func (p *Plugin) Name() string { return "url_rewrite" }

func (p *Plugin) Handle(_ context.Context, pCtx *pipeline.Ctx, w http.ResponseWriter, r *http.Request) bool {
	domain := r.Host
	for _, rule := range p.rules {
		if rule.re.MatchString(pCtx.RewrittenPath) {
			pCtx.RewrittenPath = rule.re.ReplaceAllString(pCtx.RewrittenPath, rule.replace)
			observe.PluginTriggeredTotal.WithLabelValues(domain, "url_rewrite", "rewritten").Inc()
			return true
		}
	}
	observe.PluginTriggeredTotal.WithLabelValues(domain, "url_rewrite", "pass").Inc()
	return true // rewrite is never a hard block
}

// Ensure Plugin satisfies the interface.
var _ pipeline.Plugin = (*Plugin)(nil)
