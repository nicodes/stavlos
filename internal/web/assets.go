package web

import (
	"embed"
	"io/fs"
)

// dist is the built client (web/, `make web`). It is committed, so
// `go install` needs no Node; scripts/check.sh fails when it is stale.
//
//go:embed all:dist
var dist embed.FS

func embedded() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the directory is part of the package
	}
	return sub
}
