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
// memstore/mcp -- rooted, not nested. Without reserving it, the root alias
// below would hand every well-known request to the API, and the second module
// to want one would collide with the first.
func Mount(prefix string, api http.Handler, opts ...MountOpt) *http.ServeMux {
	prefix = "/" + strings.Trim(prefix, "/")

	var cfg mountConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	top := http.NewServeMux()
	top.Handle(prefix+"/", http.StripPrefix(prefix, api))

	// The bare prefix needs registering by hand. ServeMux redirects /memstore
	// to /memstore/ only when nothing else matches it, and the root alias below
	// matches everything -- so without this, typing the base URL reaches the API
	// as a request for "/memstore" and 404s.
	top.Handle(prefix, http.RedirectHandler(prefix+"/", http.StatusMovedPermanently))

	// Reserved for the host, and now partly used. The blanket NotFound still
	// guards the space so the root alias cannot quietly answer for a well-known
	// document; the protected-resource document is registered over it on a more
	// specific pattern when OAuth is configured.
	top.Handle("/.well-known/", http.NotFoundHandler())
	if cfg.resource.Configured() {
		// GET only, and exact: the metadata document describes one resource and
		// lives at exactly one path. Serving it from the host rather than the
		// API is not a layering nicety -- the document contains absolute URLs,
		// and StripPrefix means the API cannot construct them correctly.
		top.Handle(cfg.resource.MetadataPath(), cfg.resource.MetadataHandler())
	}

	// Transition alias. Every existing client -- the Node hooks, httpclient,
	// the CLI -- addresses this daemon at the root, and they are configured in
	// several places that change on their own schedule. Serving both is what
	// lets the prefix land before the configs do. It comes out once they have.
	top.Handle("/", api)

	return top
}
