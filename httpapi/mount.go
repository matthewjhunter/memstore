package httpapi

import (
	"net/http"
	"strings"
)

// DefaultPrefix is where the daemon's own surface is mounted. It exists so a
// second service on this host has an obvious place of its own rather than
// finding memstore already occupying the root.
const DefaultPrefix = "/memstore"

// Mount composes the daemon's top-level handler.
//
// The API is mounted under prefix, and knows nothing about it: every route it
// registers is relative to /, and http.StripPrefix makes the mount point the
// host's business. That is the whole composition contract -- a second service
// (another MCP surface, a web UI) mounts the same way, and no module has to
// agree with any other about who owns which path.
//
// Two consequences worth stating, because both are easy to get wrong later.
//
// A module must never build an absolute URL from the request it is serving.
// StripPrefix rewrites the path a handler sees, so a handler that reconstructs
// its own URL reconstructs the wrong one -- it cannot see the prefix it is
// mounted under. Anything that emits an absolute URL (OAuth metadata, a
// redirect, a link in a web UI) has to be told its public base by the host.
//
// /.well-known/ is reserved here and never delegated by accident. Well-known
// URIs are root-scoped by specification and cannot be prefixed: RFC 9728 puts
// the resource path after the well-known segment, so a protected-resource
// document for /memstore/mcp lives at /.well-known/oauth-protected-resource/
// memstore/mcp -- rooted, not nested. Reserving it keeps the space the host's,
// so the second module to want a well-known document cannot collide with the
// first.
func Mount(prefix string, api http.Handler, opts ...MountOpt) *http.ServeMux {
	prefix = "/" + strings.Trim(prefix, "/")

	var cfg mountConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	top := http.NewServeMux()
	top.Handle(prefix+"/", http.StripPrefix(prefix, api))

	// The bare prefix is registered by hand so typing the base URL lands in the
	// subtree. ServeMux's implicit redirect only fires when no other pattern
	// matches, and stating it here keeps that from depending on what else the
	// host mounts later.
	top.Handle(prefix, http.RedirectHandler(prefix+"/", http.StatusMovedPermanently))

	// Reserved for the host, and partly used. The blanket NotFound guards the
	// space; the protected-resource document is registered over it on a more
	// specific pattern when OAuth is configured.
	top.Handle("/.well-known/", http.NotFoundHandler())
	if cfg.resource.Configured() {
		// GET only, and exact: the metadata document describes one resource and
		// lives at exactly one path. Serving it from the host rather than the
		// API is not a layering nicety -- the document contains absolute URLs,
		// and StripPrefix means the API cannot construct them correctly.
		top.Handle(cfg.resource.MetadataPath(), cfg.resource.MetadataHandler())
	}

	// Nothing is mounted at the root. A transition alias served the API there
	// while clients were moved onto the prefix; it was removed on 2026-08-27
	// once every configured client addressed /memstore/, and a request at the
	// root now gets the mux's own 404.
	return top
}
