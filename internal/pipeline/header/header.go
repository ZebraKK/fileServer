// Package header provides a request/response header manipulation plugin.
// Supported operations: set, add, del — applied to request headers immediately
// and to response headers staged in pCtx for the domain handler to apply.
package header

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"fileServer/internal/observe"
	"fileServer/internal/pipeline"
)

func init() {
	pipeline.RegisterFactory("header", func(cfg map[string]any) (pipeline.Plugin, error) {
		return fromConfig(cfg)
	})
}

type headerOp struct {
	op    string // "set" | "add" | "del"
	key   string
	value string
}

// Plugin applies header rules to requests and stages response header mutations.
type Plugin struct {
	requestOps  []headerOp
	responseOps []headerOp
}

func fromConfig(cfg map[string]any) (*Plugin, error) {
	p := &Plugin{}
	var err error
	if p.requestOps, err = parseOps(cfg, "request"); err != nil {
		return nil, err
	}
	if p.responseOps, err = parseOps(cfg, "response"); err != nil {
		return nil, err
	}
	return p, nil
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

func (p *Plugin) Name() string { return "header" }

func (p *Plugin) Handle(_ context.Context, pCtx *pipeline.Ctx, _ http.ResponseWriter, r *http.Request) bool {
	// Request headers: apply immediately.
	for _, op := range p.requestOps {
		applyOp(r.Header, op)
	}

	// Response headers: stage in pCtx — the domain handler applies them later.
	for _, op := range p.responseOps {
		switch op.op {
		case "set", "add":
			pCtx.SetResponseHeaders.Set(op.key, op.value)
		case "del":
			pCtx.DeleteResponseHeaders = append(pCtx.DeleteResponseHeaders, op.key)
		}
	}

	observe.PluginTriggeredTotal.WithLabelValues(r.Host, "header", "pass").Inc()
	return true
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
