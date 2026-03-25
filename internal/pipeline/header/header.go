// Package header provides a request/response header manipulation plugin.
// Supported operations: set, add, del — applied to request headers immediately
// and to response headers staged in pCtx for the domain handler to apply.
//
// The plugin is a singleton: config (ops) is parsed from the cfg parameter
// on each Execute call, enabling per-domain header rules without pre-compilation.
package header

import (
	"fmt"
	"net/http"
	"strings"

	"fileServer/internal/observe"
	"fileServer/internal/pipeline"
)

func init() {
	pipeline.RegisterPlugin("header", &Plugin{})
}

type headerOp struct {
	op    string // "set" | "add" | "del"
	key   string
	value string
}

// Plugin is the singleton header plugin. It is stateless.
type Plugin struct{}

func (p *Plugin) Execute(
	pCtx *pipeline.PipelineContext,
	cfg map[string]any,
	domain string,
	_ http.ResponseWriter,
	r *http.Request,
) bool {
	requestOps, _ := parseOps(cfg, "request")
	responseOps, _ := parseOps(cfg, "response")

	// Request headers: apply immediately.
	for _, op := range requestOps {
		applyOp(r.Header, op)
	}

	// Response headers: stage in pCtx — the domain handler applies them later.
	for _, op := range responseOps {
		switch op.op {
		case "set", "add":
			pCtx.SetResponseHeaders.Set(op.key, op.value)
		case "del":
			pCtx.DeleteResponseHeaders = append(pCtx.DeleteResponseHeaders, op.key)
		}
	}

	observe.PluginTriggeredTotal.WithLabelValues(domain, "header", "pass").Inc()
	return true
}

func parseOps(cfg map[string]any, section string) ([]headerOp, error) {
	raw, _ := cfg[section].([]any)
	var ops []headerOp
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("header: %s[%d]: expected map", section, i)
		}
		op, _ := m["op"].(string)
		key, _ := m["key"].(string)
		value, _ := m["value"].(string)
		op = strings.ToLower(op)
		if op != "set" && op != "add" && op != "del" {
			return nil, fmt.Errorf("header: %s[%d]: unknown op %q", section, i, op)
		}
		if key == "" {
			return nil, fmt.Errorf("header: %s[%d]: key is required", section, i)
		}
		ops = append(ops, headerOp{op: op, key: key, value: value})
	}
	return ops, nil
}

func applyOp(h http.Header, op headerOp) {
	switch op.op {
	case "set":
		h.Set(op.key, op.value)
	case "add":
		h.Add(op.key, op.value)
	case "del":
		h.Del(op.key)
	}
}

var _ pipeline.Plugin = (*Plugin)(nil)
