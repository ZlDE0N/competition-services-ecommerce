package main

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

// The web UI is plain HTML, CSS and JS (no build step, no CDN) embedded in the
// binary and served from the same origin as the API, so the strict CSP applies.

//go:embed web
var webFS embed.FS

func registerUI(router *gin.Engine) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	index, _ := fs.ReadFile(sub, "index.html")
	router.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", index)
	})
	router.GET("/favicon.svg", func(c *gin.Context) {
		data, _ := fs.ReadFile(sub, "favicon.svg")
		c.Header("Cache-Control", "public, max-age=86400")
		c.Data(http.StatusOK, "image/svg+xml", data)
	})
	router.StaticFS("/assets", http.FS(mustSub(sub, "assets")))
}

func mustSub(f fs.FS, dir string) fs.FS {
	s, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return s
}
