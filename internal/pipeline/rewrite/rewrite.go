// Package rewrite provides a URL-rewriting pipeline plugin.
// Rules are evaluated in order; the first match wins and rewrites pCtx.RewrittenPath.
//
// The plugin is a singleton: config (rules) is parsed from the cfg parameter
// on each Execute call, enabling per-domain rule sets without pre-compilation.
package rewrite

import (
	"fmt"
	"net/http"
	"regexp"

	"fileServer/internal/observe"
	"fileServer/internal/pipeline"
)

func init() {
	pipeline.RegisterPlugin("url_rewrite", &Plugin{})
}

// Plugin is the singleton URL-rewrite plugin. It is stateless.
type Plugin struct{}

func (p *Plugin) Execute(
	pCtx *pipeline.PipelineContext,
	cfg map[string]any,
	domain string,
	_ http.ResponseWriter,
	_ *http.Request,
) bool {
	rawRules, _ := cfg["rules"].([]any)
	for i, raw := range rawRules {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		match, _ := m["match"].(string)
		replace, _ := m["replace"].(string)
		if match == "" {
			continue
		}
		re, err := regexp.Compile(match)
		if err != nil {
			// Misconfigured rule — log and skip.
			_ = fmt.Sprintf("url_rewrite: rule[%d]: invalid regex %q: %v", i, match, err)
			continue
		}
		if re.MatchString(pCtx.RewrittenPath) {
			pCtx.RewrittenPath = re.ReplaceAllString(pCtx.RewrittenPath, replace)
			observe.PluginTriggeredTotal.WithLabelValues(domain, "url_rewrite", "rewritten").Inc()
			return true
		}
	}
	observe.PluginTriggeredTotal.WithLabelValues(domain, "url_rewrite", "pass").Inc()
	return true // rewrite is never a hard block
}

// Ensure Plugin satisfies the interface.
var _ pipeline.Plugin = (*Plugin)(nil)
