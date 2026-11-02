package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "224.0.0.1", "::1", "fe80::1",
		"fc00::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1",
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "172.32.0.1"}
	for _, s := range allowed {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	if !isBlockedIP(nil) {
		t.Error("nil IP must be blocked")
	}
}

func TestValidatePublicURL(t *testing.T) {
	ok := []string{"https://example.com/a.png", "http://example.com:8080/x?y=1"}
	for _, u := range ok {
		if _, err := validatePublicURL(u); err != nil {
			t.Errorf("%s: unexpected error %v", u, err)
		}
	}
	bad := []string{"", "ftp://example.com/x", "file:///etc/passwd", "gopher://x", "http://", "//example.com", "https://user:pw@example.com/", "javascript:alert(1)"}
	for _, u := range bad {
		if _, err := validatePublicURL(u); err == nil {
			t.Errorf("%q should be rejected", u)
		}
	}
}

func TestSafeClientRefusesLoopbackAndRedirectsToIt(t *testing.T) {
	hit := false
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true; w.Write([]byte("secret")) }))
	defer internal.Close()

	client := newSafeHTTPClient(2 * time.Second)
	if _, err := safeGet(client, internal.URL); err == nil || !errors.Is(err, errBlockedAddress) && !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("direct loopback fetch must be blocked, got %v", err)
	}
	if hit {
		t.Fatal("the internal server must never receive the request")
	}
}

func TestSafeDialContextBlocksLiteralInternalAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:80", "[::1]:80", "169.254.169.254:80", "10.0.0.5:443"} {
		_, err := safeDialContext(context.Background(), "tcp", addr)
		if err == nil || !errors.Is(err, errBlockedAddress) {
			t.Errorf("%s: want errBlockedAddress, got %v", addr, err)
		}
	}
}

func TestReadLimited(t *testing.T) {
	if _, err := readLimited(strings.NewReader(strings.Repeat("a", 11)), 10); !errors.Is(err, errTooLarge) {
		t.Errorf("want errTooLarge, got %v", err)
	}
	got, err := readLimited(strings.NewReader("hello"), 10)
	if err != nil || string(got) != "hello" {
		t.Errorf("got %q %v", got, err)
	}
}

func TestAuthConfig(t *testing.T) {
	if enforce, problem := authConfig("production", []string{"k"}, false); !enforce || problem != "" {
		t.Error("keys configured -> enforce")
	}
	if _, problem := authConfig("production", nil, false); problem == "" {
		t.Error("production without keys must refuse to start")
	}
	if _, problem := authConfig("production", nil, true); problem != "" {
		t.Error("explicit opt-out must be honoured")
	}
	if enforce, problem := authConfig("development", nil, false); enforce || problem != "" {
		t.Error("development without keys runs open")
	}
}

func authRouter(keys ...string) *gin.Engine {
	r := gin.New()
	r.Use(apiKeyAuth(keys))
	ok := func(c *gin.Context) { c.String(200, "ok") }
	r.GET("/products/1", ok)
	r.GET("/health", ok)
	r.GET("/demo/catalog", ok)
	r.GET("/", ok)
	r.OPTIONS("/products/1", ok)
	return r
}

func do(r *gin.Engine, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAPIKeyAuth(t *testing.T) {
	r := authRouter("alpha", "beta")
	if w := do(r, "GET", "/products/1", nil); w.Code != 401 {
		t.Errorf("no key: %d", w.Code)
	}
	if w := do(r, "GET", "/products/1", map[string]string{"X-API-Key": "nope"}); w.Code != 401 {
		t.Errorf("wrong key: %d", w.Code)
	}
	for _, k := range []string{"alpha", "beta"} {
		if w := do(r, "GET", "/products/1", map[string]string{"X-API-Key": k}); w.Code != 200 {
			t.Errorf("key %s: %d", k, w.Code)
		}
	}
	for _, p := range []string{"/health", "/demo/catalog", "/"} {
		if w := do(r, "GET", p, nil); w.Code != 200 {
			t.Errorf("%s must be public: %d", p, w.Code)
		}
	}
	if w := do(r, "OPTIONS", "/products/1", nil); w.Code != 200 {
		t.Errorf("preflight must pass the auth layer: %d", w.Code)
	}
	if w := do(r, "GET", "/products/1", map[string]string{"X-API-Key": ""}); w.Code != 401 {
		t.Errorf("empty key: %d", w.Code)
	}
}

func TestPublicPathDoesNotLeakPrefixes(t *testing.T) {
	for _, p := range []string{"/demoX", "/assetsX", "/healthz", "/demo", "/products/../demo/x"} {
		if publicPath(p) {
			t.Errorf("%s must not be public", p)
		}
	}
}

func TestCORSAllowList(t *testing.T) {
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://crm.example.com, https://shop.example.com/")
	r := gin.New()
	r.Use(corsMiddleware())
	r.GET("/x", func(c *gin.Context) { c.String(200, "ok") })

	w := do(r, "GET", "/x", map[string]string{"Origin": "https://crm.example.com"})
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://crm.example.com" {
		t.Errorf("allowed origin not echoed: %q", got)
	}
	if w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials must not be advertised")
	}
	w = do(r, "GET", "/x", map[string]string{"Origin": "https://shop.example.com"})
	if w.Header().Get("Access-Control-Allow-Origin") != "https://shop.example.com" {
		t.Error("trailing slash in config must still match")
	}
	w = do(r, "GET", "/x", map[string]string{"Origin": "https://evil.example.net"})
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("unlisted origin must get no CORS headers")
	}
	if w.Header().Get("Vary") != "Origin" {
		t.Error("Vary: Origin missing")
	}
	w = do(r, "OPTIONS", "/x", map[string]string{"Origin": "https://crm.example.com"})
	if w.Code != 204 {
		t.Errorf("preflight status %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	r := gin.New()
	r.Use(securityHeaders())
	r.GET("/", func(c *gin.Context) { c.String(200, "ok") })
	h := do(r, "GET", "/", nil).Header()
	for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if h.Get(k) != want {
			t.Errorf("%s = %q", k, h.Get(k))
		}
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") || strings.Contains(h.Get("Content-Security-Policy"), "unsafe-inline") {
		t.Errorf("CSP too loose: %s", h.Get("Content-Security-Policy"))
	}
}

func TestActorEmailIgnoresBody(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"reviewedBy":"admin@example.com"}`))
	if got := actorEmail(c); got != "" {
		t.Errorf("identity must not come from the body, got %q", got)
	}
	c.Request.Header.Set("X-User-Email", "  reviewer@example.com ")
	if got := actorEmail(c); got != "reviewer@example.com" {
		t.Errorf("got %q", got)
	}
}
