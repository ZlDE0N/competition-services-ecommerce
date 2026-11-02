package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// Security layer: API-key authentication, an explicit CORS allow-list and
// browser hardening headers. Everything is configured with environment
// variables; the defaults are the safe ones.
//
//	API_KEYS               comma-separated keys accepted in the X-API-Key header
//	ALLOW_UNAUTHENTICATED  "true" lets the API run without keys in production
//	CORS_ALLOWED_ORIGINS   comma-separated origins allowed to call the API from a browser

// publicPath reports whether a request needs no API key: the web UI, the
// health probe and the self-contained demo (which never touches real data).
func publicPath(path string) bool {
	switch {
	case path == "/", path == "/health", path == "/favicon.ico", path == "/favicon.svg":
		return true
	case strings.HasPrefix(path, "/assets/"), strings.HasPrefix(path, "/demo/"):
		return true
	}
	return false
}

func splitList(raw string) []string {
	var out []string
	for _, v := range strings.Split(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// keyDigests hashes the configured keys so the comparison below always works
// on equal-length values.
func keyDigests(keys []string) [][32]byte {
	out := make([][32]byte, 0, len(keys))
	for _, k := range keys {
		out = append(out, sha256.Sum256([]byte(k)))
	}
	return out
}

func keyAllowed(digests [][32]byte, presented string) bool {
	got := sha256.Sum256([]byte(presented))
	ok := 0
	for _, d := range digests { // no early exit: time does not depend on which key matched
		ok |= subtle.ConstantTimeCompare(got[:], d[:])
	}
	return ok == 1
}

// authConfig decides how the API is protected for this process.
//   - keys configured            -> every non-public route needs a valid X-API-Key
//   - no keys, production        -> refuse to start unless ALLOW_UNAUTHENTICATED=true
//   - no keys, any other env     -> open, with a warning (local development)
func authConfig(env string, keys []string, allowOpen bool) (enforce bool, err string) {
	if len(keys) > 0 {
		return true, ""
	}
	if strings.EqualFold(env, "production") && !allowOpen {
		return false, "ENVIRONMENT=production requires API_KEYS (or ALLOW_UNAUTHENTICATED=true to run open on purpose)"
	}
	return false, ""
}

// apiKeyAuth enforces X-API-Key on non-public routes.
func apiKeyAuth(keys []string) gin.HandlerFunc {
	digests := keyDigests(keys)
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions || publicPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		if !keyAllowed(digests, c.GetHeader("X-API-Key")) {
			c.Header("WWW-Authenticate", `ApiKey realm="catalog-enrichment"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid API key"})
			return
		}
		c.Next()
	}
}

// actorEmail is the identity of the person on whose behalf a trusted caller (a
// holder of an API key) is acting. It comes ONLY from the X-User-Email header
// the caller sets after authenticating its own user; a value in a request body
// is never used to authorize anything. Roles are then checked against the DB.
func actorEmail(c *gin.Context) string {
	return strings.TrimSpace(c.GetHeader("X-User-Email"))
}

// requireApprover lets only users with an approval role (checked in the DB)
// through. Use it on every route that changes configuration or content.
func requireApprover() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !hasQueueApprovalRole(actorEmail(c)) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "an approval role is required (X-User-Email must belong to a Super Admin or Products Admin)",
			})
			return
		}
		c.Next()
	}
}

// corsMiddleware answers browser calls only for origins on the allow-list.
// It never answers with "*" together with credentials.
func corsMiddleware() gin.HandlerFunc {
	allowed := map[string]bool{}
	for _, o := range splitList(os.Getenv("CORS_ALLOWED_ORIGINS")) {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		c.Header("Vary", "Origin")
		if origin != "" && allowed[strings.TrimRight(origin, "/")] {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, PATCH, DELETE")
			c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-API-Key, X-User-Email")
			c.Header("Access-Control-Max-Age", "86400")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// securityHeaders adds the headers every response should carry. The CSP is
// strict on purpose: the UI is self-contained (no CDN, no inline script).
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: https:; style-src 'self'; script-src 'self'; "+
				"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		c.Next()
	}
}

// setupSecurity wires the layer into the router and applies the startup rule.
func setupSecurity(router *gin.Engine) {
	router.Use(securityHeaders(), corsMiddleware())
	keys := splitList(os.Getenv("API_KEYS"))
	enforce, problem := authConfig(os.Getenv("ENVIRONMENT"), keys, strings.EqualFold(os.Getenv("ALLOW_UNAUTHENTICATED"), "true"))
	if problem != "" {
		log.Fatalf("🔒 %s", problem)
	}
	if enforce {
		router.Use(apiKeyAuth(keys))
		log.Printf("🔒 API key authentication enabled (%d key(s))", len(keys))
		return
	}
	log.Printf("⚠️  API running WITHOUT authentication (set API_KEYS before exposing it)")
}
