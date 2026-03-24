// Package pipeline defines the plugin interface and the ordered execution chain
// applied to every incoming request before it reaches the cache/origin layer.
package pipeline

import (
	"context"
	"fmt"
	"net/http"

	"fileServer/internal/config"
)

// Ctx carries mutable state that plugins can read from and write to.
// It is created fresh for each request.
type Ctx struct {
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

// Plugin is the interface every middleware plugin must implement.
type Plugin interface {
	// Name returns the plugin identifier (used in metrics / logs).
	Name() string

	// Handle executes the plugin logic. Return true to continue to the next
	// plugin, false to short-circuit (the plugin must have already written a
	// response, e.g. 429 for rate limiting).
	Handle(ctx context.Context, pCtx *Ctx, w http.ResponseWriter, r *http.Request) bool
}

// Pipeline executes an ordered list of plugins for each request.
type Pipeline struct {
	plugins []Plugin
}

// Execute runs each plugin in order. Returns false as soon as any plugin
// short-circuits (returns false); returns true if all plugins passed.
func (p *Pipeline) Execute(ctx context.Context, pCtx *Ctx, w http.ResponseWriter, r *http.Request) bool {
	for _, pl := range p.plugins {
		if !pl.Handle(ctx, pCtx, w, r) {
			return false
		}
	}
	return true
}

// Factory knows how to instantiate plugins by type name.
// Each plugin package registers itself via RegisterFactory.
var factories = map[string]func(cfg map[string]any) (Plugin, error){}

// RegisterFactory registers a constructor for a plugin type. Called from each
// plugin package's init().
func RegisterFactory(typeName string, fn func(cfg map[string]any) (Plugin, error)) {
	factories[typeName] = fn
}

// Build constructs a Pipeline from a slice of PluginConfig.
// Returns an error if any plugin type is unknown or misconfigured.
func Build(cfgs []config.PluginConfig) (*Pipeline, error) {
	plugins := make([]Plugin, 0, len(cfgs))
	for _, pc := range cfgs {
		fn, ok := factories[pc.Type]
		if !ok {
			return nil, fmt.Errorf("pipeline: unknown plugin type %q", pc.Type)
		}
		pl, err := fn(pc.Config)
		if err != nil {
			return nil, fmt.Errorf("pipeline: init plugin %q: %w", pc.Type, err)
		}
		plugins = append(plugins, pl)
	}
	return &Pipeline{plugins: plugins}, nil
}

// NewPipelineCtx creates a fresh Ctx for a request, seeding RewrittenPath.
func NewPipelineCtx(originalPath string) *Ctx {
	return &Ctx{
		RewrittenPath:      originalPath,
		SetResponseHeaders: make(http.Header),
	}
}

// ApplyResponseMutations writes staged header mutations to w just before the
// response is sent. Called by the domain handler after executing the pipeline.
func ApplyResponseMutations(w http.ResponseWriter, pCtx *Ctx) {
	for k, vs := range pCtx.SetResponseHeaders {
		for _, v := range vs {
			w.Header().Set(k, v)
		}
	}
	for _, k := range pCtx.DeleteResponseHeaders {
		w.Header().Del(k)
	}
}
