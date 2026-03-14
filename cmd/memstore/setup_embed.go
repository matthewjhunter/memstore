package main

import "embed"

// hookFS contains the hook scripts from cmd/memstore/hooks/.
// These are embedded at build time so `memstore setup` works after
// `go install` without needing the source tree.
//
//go:embed hooks/*
var hookFS embed.FS
