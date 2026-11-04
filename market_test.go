package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestMarketSnapshotIsHonest(t *testing.T) {
	s := marketArgentina2026
	if s.Country == "" || s.AsOf == "" || len(s.Caveat) < 40 {
		t.Fatal("snapshot needs country, as-of date and a real caveat")
	}
	check := func(where string, m marketMetric) {
		u, err := url.Parse(m.Source)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			t.Errorf("%s %q: source must be an https URL, got %q", where, m.Label, m.Source)
		}
		if strings.TrimSpace(m.Value) == "" || strings.TrimSpace(m.Label) == "" {
			t.Errorf("%s: empty label or value", where)
		}
		if len(strings.TrimSpace(m.Period)) < 4 {
			t.Errorf("%s %q: a figure needs the period it describes", where, m.Label)
		}
		if m.Quality != "high" && m.Quality != "medium" && m.Quality != "low" {
			t.Errorf("%s %q: quality %q", where, m.Label, m.Quality)
		}
	}
	for _, m := range s.Totals {
		check("totals", m)
	}
	ids := map[string]bool{}
	for _, p := range s.Players {
		if ids[p.ID] || p.ID == "" {
			t.Errorf("duplicate or empty player id %q", p.ID)
		}
		ids[p.ID] = true
		if !validKinds[p.Kind] {
			t.Errorf("%s: kind %q", p.ID, p.Kind)
		}
		if p.Threat != "high" && p.Threat != "medium" && p.Threat != "low" {
			t.Errorf("%s: threat %q", p.ID, p.Threat)
		}
		if len(p.Summary) < 20 || len(p.Playbook) < 20 || len(p.Metrics) == 0 {
			t.Errorf("%s: incomplete entry", p.ID)
		}
		for _, m := range p.Metrics {
			check(p.ID, m)
		}
	}
	if len(s.Players) < 6 {
		t.Errorf("expected at least 6 players, got %d", len(s.Players))
	}
}

func TestMarketLowQualityNeverStandsAlone(t *testing.T) {
	// A player whose only evidence is low quality must say why in a note.
	for _, p := range marketArgentina2026.Players {
		allLow := true
		for _, m := range p.Metrics {
			if m.Quality != "low" {
				allLow = false
			}
		}
		if allLow {
			for _, m := range p.Metrics {
				if p.Threat == "high" {
					t.Errorf("%s: a high threat rating cannot rest on low-quality data only (%q)", p.ID, m.Label)
				}
			}
		}
	}
}

func TestMarketEndpoint(t *testing.T) {
	r := demoRouter()
	w, body := send(r, "GET", "/demo/market", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if body["country"] != "Argentina" || body["players"] == nil || body["totals"] == nil {
		t.Errorf("shape: %v", body)
	}
	if w, _ := send(r, "GET", "/demo/market?country=AR", ""); w.Code != 200 {
		t.Errorf("country is case-insensitive: %d", w.Code)
	}
	if w, _ := send(r, "GET", "/demo/market?country=zz", ""); w.Code != 404 {
		t.Errorf("unknown country: %d", w.Code)
	}
	if w, _ := send(r, "POST", "/demo/market", ""); w.Code == 200 {
		t.Error("read-only endpoint must not accept POST")
	}
	// reachable without an API key, like the rest of /demo
	w, _ = send(r, "GET", "/demo/market", "")
	if w.Code != 200 {
		t.Errorf("public route needs no key: %d", w.Code)
	}
}
