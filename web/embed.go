// Package web embeds the built SPA (web/dist) so the gateway ships as a single binary.
// dist 目录不存在时编译仍可通过（embed 目录需存在文件，这里用占位保证）。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist 返回 web/dist 子文件系统（index.html 在根）。
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return distFS
	}
	return sub
}
