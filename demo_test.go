package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func demoRouter() *gin.Engine {
	demo.mu.Lock()
	demo.queue, demo.seq = nil, 0
	demo.mu.Unlock()
	r := gin.New()
	r.Use(securityHeaders(), apiKeyAuth([]string{"secret"}))
	registerUI(r)
	demoRegister(r)
	r.GET("/products/1", func(c *gin.Context) { c.String(200, "private") })
	return r
}

func send(r *gin.Engine, method, path, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestUIIsServedWithoutAKeyAndWithStrictCSP(t *testing.T) {
	r := demoRouter()
	for _, p := range []string{"/", "/favicon.svg", "/assets/styles.css", "/assets/app.js", "/demo/info", "/demo/catalog"} {
		w, _ := send(r, "GET", p, "")
		if w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	w, _ := send(r, "GET", "/", "")
	if !strings.Contains(w.Body.String(), "Competition Services") || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Error("index not served as HTML")
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP: %s", csp)
	}
	if w, _ := send(r, "GET", "/products/1", ""); w.Code != 401 {
		t.Errorf("private route must still need a key: %d", w.Code)
	}
}

func TestUIHasNoInlineScriptOrStyle(t *testing.T) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, bad := range []string{" style=", "<style", "onclick=", "onload=", "javascript:", "http://", "//cdn", "googleapis"} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html contains %q, which the CSP or the privacy rules forbid", bad)
		}
	}
	if n := strings.Count(html, "<script"); n != 1 || !strings.Contains(html, `<script src="/assets/app.js" defer></script>`) {
		t.Errorf("expected exactly one external script, found %d", n)
	}
}

func TestDemoEnrichQueueAndApprove(t *testing.T) {
	r := demoRouter()
	_, cat := send(r, "GET", "/demo/catalog", "")
	items := cat["items"].([]interface{})
	if len(items) != len(demoCatalog) {
		t.Fatalf("catalog has %d items", len(items))
	}
	first := items[0].(map[string]interface{})
	if first["status"] != "raw" {
		t.Errorf("fresh catalog must be raw, got %v", first["status"])
	}

	w, res := send(r, "POST", "/demo/enrich", `{"productId":"demo-001"}`)
	if w.Code != 200 {
		t.Fatalf("enrich: %d %s", w.Code, w.Body.String())
	}
	for _, k := range []string{"originalData", "enhancedData", "confidence", "processingTime", "ruleApplied", "stages", "completeness", "queueId"} {
		if _, ok := res[k]; !ok {
			t.Errorf("response is missing %q (it must mirror the real API)", k)
		}
	}
	comp := res["completeness"].(map[string]interface{})
	if comp["after"].(float64) <= comp["before"].(float64) {
		t.Errorf("enrichment must raise completeness: %v", comp)
	}
	qid := res["queueId"].(string)

	// same product again: still ONE pending item (same rule as the real queue)
	send(r, "POST", "/demo/enrich", `{"productId":"demo-001"}`)
	_, q := send(r, "GET", "/demo/queue?status=pending", "")
	if int(q["total"].(float64)) != 1 {
		t.Errorf("duplicate pending items: %v", q["total"])
	}

	if w, _ := send(r, "POST", "/demo/queue/"+qid+"/approve", `{}`); w.Code != 200 {
		t.Errorf("approve: %d", w.Code)
	}
	if w, _ := send(r, "POST", "/demo/queue/"+qid+"/approve", `{}`); w.Code != 409 {
		t.Errorf("second review must conflict: %d", w.Code)
	}
	_, ap := send(r, "GET", "/demo/queue?status=approved", "")
	item := ap["items"].([]interface{})[0].(map[string]interface{})
	if item["status"] != "approved" || item["enrichedData"] == nil {
		t.Errorf("approved item shape: %v", item)
	}
	_, cat = send(r, "GET", "/demo/catalog", "")
	if cat["items"].([]interface{})[0].(map[string]interface{})["status"] != "approved" {
		t.Error("catalog must reflect the approval")
	}
}

func TestDemoRejectKeepsReasonAndInputIsValidated(t *testing.T) {
	r := demoRouter()
	_, res := send(r, "POST", "/demo/enrich", `{"productId":"demo-002"}`)
	qid := res["queueId"].(string)
	send(r, "POST", "/demo/queue/"+qid+"/reject", `{"reason":"too salesy"}`)
	_, rj := send(r, "GET", "/demo/queue?status=rejected", "")
	if got := rj["items"].([]interface{})[0].(map[string]interface{})["rejectionReason"]; got != "too salesy" {
		t.Errorf("reason lost: %v", got)
	}
	for body, want := range map[string]int{`{}`: 400, `not json`: 400, `{"productId":"nope"}`: 404, `{"productId":""}`: 400} {
		if w, _ := send(r, "POST", "/demo/enrich", body); w.Code != want {
			t.Errorf("body %q: got %d want %d", body, w.Code, want)
		}
	}
	if w, _ := send(r, "POST", "/demo/queue/q-999/approve", `{}`); w.Code != 404 {
		t.Errorf("unknown queue id: %d", w.Code)
	}
	big := `{"productId":"` + strings.Repeat("a", 10000) + `"}`
	if w, _ := send(r, "POST", "/demo/enrich", big); w.Code == 200 {
		t.Error("oversized body must be rejected")
	}
}

func TestDemoReset(t *testing.T) {
	r := demoRouter()
	send(r, "POST", "/demo/enrich", `{"productId":"demo-003"}`)
	send(r, "POST", "/demo/reset", `{}`)
	_, q := send(r, "GET", "/demo/queue?status=all", "")
	if int(q["total"].(float64)) != 0 {
		t.Errorf("reset left %v items", q["total"])
	}
}

func TestDemoContentIsSelfConsistentAndSafe(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range demoCatalog {
		if seen[p.ID] {
			t.Errorf("duplicate id %s", p.ID)
		}
		seen[p.ID] = true
		if p.after() <= p.before() {
			t.Errorf("%s: after (%d) must beat before (%d)", p.ID, p.after(), p.before())
		}
		if p.Confidence <= 0 || p.Confidence > 1 {
			t.Errorf("%s: confidence %v", p.ID, p.Confidence)
		}
		if sanitizeHTML(p.Short) != p.Short || sanitizeHTML(p.Full) != p.Full {
			t.Errorf("%s: sample HTML is not clean under our own sanitizer", p.ID)
		}
	}
}

func TestDemoQueueIsBounded(t *testing.T) {
	r := demoRouter()
	for i := 0; i < maxDemoQueue*3; i++ { // enrich + approve over and over: the abuse pattern
		_, res := send(r, "POST", "/demo/enrich", `{"productId":"demo-001"}`)
		send(r, "POST", "/demo/queue/"+res["queueId"].(string)+"/approve", `{}`)
	}
	demo.mu.Lock()
	n := len(demo.queue)
	demo.mu.Unlock()
	if n > maxDemoQueue {
		t.Fatalf("queue grew to %d (limit %d)", n, maxDemoQueue)
	}
	_, res := send(r, "POST", "/demo/enrich", `{"productId":"demo-002"}`)
	if res["queueId"] == nil {
		t.Error("enrich must keep working at the limit")
	}
	seen := map[string]bool{}
	demo.mu.Lock()
	for _, q := range demo.queue {
		if seen[q.ID] {
			t.Errorf("duplicate queue id %s", q.ID)
		}
		seen[q.ID] = true
	}
	demo.mu.Unlock()
}

func TestDemoEnabledRules(t *testing.T) {
	cases := []struct {
		env, mode string
		want      bool
	}{
		{"", "", true}, {"development", "", true}, {"production", "", false},
		{"production", "true", true}, {"development", "false", false}, {"production", "TRUE", true},
	}
	for _, c := range cases {
		t.Setenv("ENVIRONMENT", c.env)
		t.Setenv("DEMO_MODE", c.mode)
		if got := demoEnabled(); got != c.want {
			t.Errorf("ENVIRONMENT=%q DEMO_MODE=%q: got %v want %v", c.env, c.mode, got, c.want)
		}
	}
}

func TestDemoIDFormat(t *testing.T) {
	for n, want := range map[int]string{1: "q-001", 9: "q-009", 10: "q-010", 42: "q-042", 100: "q-100", 999: "q-999", 1000: "q-1000", 12345: "q-12345"} {
		if got := demoID(n); got != want {
			t.Errorf("demoID(%d)=%s want %s", n, got, want)
		}
	}
	_ = http.StatusOK
}
