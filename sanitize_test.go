package main

import (
	"strings"
	"testing"
	"time"
)

func TestSanitizeHTML(t *testing.T) {
	cases := map[string]string{
		"plain text":                              "plain text",
		"<p>Hola <strong>mundo</strong></p>":      "<p>Hola <strong>mundo</strong></p>",
		"<ul><li>uno</li><li>dos</li></ul>":       "<ul><li>uno</li><li>dos</li></ul>",
		`<p onclick="x()">hi</p>`:                 "<p>hi</p>",
		`<a href="javascript:alert(1)">click</a>`: "click",
		`<img src=x onerror=alert(1)>`:            "",
		"<script>alert(1)</script>texto":          "texto",
		"<style>p{}</style><p>a</p>":              "<p>a</p>",
		"<iframe src=//evil></iframe>ok":          "ok",
		"1 < 2 & 3 > 2":                           "1 &lt; 2 &amp; 3 &gt; 2",
		"AT&T":                                    "AT&amp;T",
		"<b>bold</b><br>line":                     "<b>bold</b><br>line",
		"<p>unclosed":                             "<p>unclosed",
		`<svg/onload=alert(1)>`:                   "",
		"&lt;script&gt;alert(1)&lt;/script&gt;":   "&lt;script&gt;alert(1)&lt;/script&gt;",
		"<h3>Título</h3>":                         "<h3>Título</h3>",
		"<SCRIPT>x</SCRIPT>ok":                    "ok",
	}
	for in, want := range cases {
		if got := sanitizeHTML(in); got != want {
			t.Errorf("sanitizeHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeHTMLIsIdempotent(t *testing.T) {
	for _, in := range []string{"<p>a &amp; b</p>", "x < y", "<ul><li>1</li></ul>", "AT&T <script>x</script>"} {
		once := sanitizeHTML(in)
		if twice := sanitizeHTML(once); twice != once {
			t.Errorf("not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func TestStripHTML(t *testing.T) {
	if got := stripHTML("<b>Taladro</b> 18V <script>x</script>&amp; case"); got != "Taladro 18V & case" {
		t.Errorf("got %q", got)
	}
	if got := stripHTML("  sin tags  "); got != "sin tags" {
		t.Errorf("got %q", got)
	}
}

func TestStripHTMLNeverReturnsMarkup(t *testing.T) {
	for _, in := range []string{
		"&lt;script&gt;alert(1)&lt;/script&gt;", "&#60;img src=x onerror=alert(1)&#62;",
		"<<script>script>alert(1)", "a &amp;lt;b&amp;gt; c", "x<y",
	} {
		got := stripHTML(in)
		if strings.ContainsAny(got, "<>") {
			t.Errorf("stripHTML(%q) = %q still contains angle brackets", in, got)
		}
	}
}

func TestSanitizeJSONStrings(t *testing.T) {
	out, err := sanitizeJSONStrings([]byte(`{"enhancedData":{"name":"X","fullDescription":"<p>ok</p><script>bad()</script>","tags":["<b>a</b>","<img src=x onerror=1>"]},"n":3,"flag":true}`))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"<script", "onerror", "<img"} {
		if strings.Contains(s, bad) {
			t.Errorf("%q survived: %s", bad, s)
		}
	}
	if !strings.Contains(s, "<p>ok</p>") || !strings.Contains(s, `"n":3`) || !strings.Contains(s, `"flag":true`) {
		t.Errorf("valid content was damaged: %s", s)
	}
	for _, bad := range []string{``, `not json`, `[1,2]`, `"string"`, `null`, strings.Repeat("a", maxEditedDataBytes+1)} {
		if _, err := sanitizeJSONStrings([]byte(bad)); err == nil {
			t.Errorf("%.20q should be rejected", bad)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike(`50%_off[x]\`); got != `50\%\_off\[x]\\` {
		t.Errorf("got %q", got)
	}
}

func TestCatalogLocalNowOffset(t *testing.T) {
	diff := func(env string) float64 {
		t.Setenv("CATALOG_UTC_OFFSET_HOURS", env)
		return catalogLocalNow().Sub(time.Now().UTC()).Hours()
	}
	round := func(h float64) int { return int(h + 0.5*sign(h)) }
	if got := round(diff("")); got != -5 {
		t.Errorf("default offset: %d", got)
	}
	if got := round(diff("2")); got != 2 {
		t.Errorf("offset 2: %d", got)
	}
	for _, bad := range []string{"abc", "99", "-99"} {
		if got := round(diff(bad)); got != -5 {
			t.Errorf("invalid %q must fall back to -5, got %d", bad, got)
		}
	}
}

func sign(h float64) float64 {
	if h < 0 {
		return -1
	}
	return 1
}
