// Package pipeline defines the plugin interface and the ordered execution chain
// applied to every incoming request before it reaches the cache/origin layer.
package pipeline

import (
	"context"
	"net/http"

	"fileServer/internal/config"
)

// PipelineContext carries mutable state that plugins can read from and write to.
// It is created fresh for each request.
type PipelineContext struct {
	// RewrittenPath holds the URL path after the url_rewrite plugin runs.
	// Downstream layers (cache key, origin pull) must use this value.
	// If no rewrite plugin is active it equals the original r.URL.Path.
	RewrittenPath string

	// SetResponseHeaders lists response headers to inject/overwrite.
	// Applied by the domain handler before writing the response.
	SetResponseHeaders http.Header

	// DeleteResponseHeaders lists response header names to remove.
	// Applied by the domain handler before writing the response.
	DeleteResponseHeaders []string
}

// ctxKey is the unexported key used to store PipelineContext in a request context.
type ctxKey struct{}

// PipelineCtxFrom retrieves the PipelineContext stored in ctx by Execute.
// Returns (nil, false) if the context does not contain one.
func PipelineCtxFrom(ctx context.Context) (*PipelineContext, bool) {
	v, ok := ctx.Value(ctxKey{}).(*PipelineContext)
	return v, ok
}

// Plugin is the interface every middleware plugin must implement.
// Plugins are registered as singletons and must be safe for concurrent use.
//
// Execute runs the plugin logic for a single request.
//   - pCtx is the shared per-request context; plugins may mutate it.
//   - cfg is the plugin's raw configuration for the current domain.
//   - domain is the virtual-host name of the request.
//   - w is the response writer (plugins that short-circuit must write a response).
//   - r is the incoming request.
//
// Return true to pass to the next plugin, false to short-circuit.
type Plugin interface {
	Execute(pCtx *PipelineContext, cfg map[string]any, domain string, w http.ResponseWriter, r *http.Request) bool
}

// registry maps plugin type names to singleton Plugin instances.
// Populated by plugin packages via RegisterPlugin in their init() functions.
var registry = map[string]Plugin{}

// RegisterPlugin registers a singleton plugin for a given type name.
// Typically called from a plugin package's init().
func RegisterPlugin(typeName string, p Plugin) {
	registry[typeName] = p
}

// Pipeline executes an ordered list of plugins for each request.
// It holds a reference to the global plugin registry and is safe for concurrent use.
type Pipeline struct {
	reg map[string]Plugin
}

// New creates a Pipeline backed by the global plugin registry.
func New() *Pipeline {
	return &Pipeline{reg: registry}
}

// Execute runs the plugin chain described by pluginConfigs.
//
// It returns:
//   - pCtx: the populated PipelineContext (always non-nil).
//   - r: the request updated with pCtx stored in its context (for cross-layer access).
//   - ok: false if any plugin short-circuited (the plugin wrote its own response).
func (p *Pipeline) Execute(
	pluginConfigs []config.PluginConfig,
	domain string,
	w http.ResponseWriter,
	r *http.Request,
) (*PipelineContext, *http.Request, bool) {
	pCtx := &PipelineContext{
		RewrittenPath:      r.URL.Path,
		SetResponseHeaders: make(http.Header),
	}
	for _, pcfg := range pluginConfigs {
		pl, ok := p.reg[pcfg.Type]
		if !ok {
			// Unknown plugin type: skip silently (misconfiguration is caught at startup).
			continue
		}
		if !pl.Execute(pCtx, pcfg.Config, domain, w, r) {
			return pCtx, r, false
		}
	}
	// Store pCtx in the request context for cross-layer access (e.g. middleware, sub-handlers).
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, pCtx))
	return pCtx, r, true
}

// ApplyResponseMutations writes staged header mutations to w just before the
// response is sent. Called by the domain handler after executing the pipeline.
func ApplyResponseMutations(w http.ResponseWriter, pCtx *PipelineContext) {
	for k, vs := range pCtx.SetResponseHeaders {
		for _, v := range vs {
			w.Header().Set(k, v)
		}
	}
	for _, k := range pCtx.DeleteResponseHeaders {
		w.Header().Del(k)
	}
}
