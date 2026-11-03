package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClaude points the shared client at a local server for the test and
// removes the real waiting. It records the delays the client asked for.
func fakeClaude(t *testing.T, h http.HandlerFunc) (*atomic.Int32, *[]time.Duration) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	var delays []time.Duration
	oldURL, oldSleep := claudeURL, claudeSleep
	claudeURL = srv.URL
	claudeSleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return ctx.Err()
	}
	t.Cleanup(func() { claudeURL, claudeSleep = oldURL, oldSleep })
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	return &hits, &delays
}

const okBody = `{"content":[{"type":"text","text":"hola"}],"usage":{"input_tokens":3,"output_tokens":1}}`

func TestPostClaudeSuccessSendsTheRightRequest(t *testing.T) {
	var gotKey, gotVersion, gotType, gotBeta, gotBody string
	hits, _ := fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotVersion, gotType = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.Header.Get("Content-Type")
		gotBeta = r.Header.Get("anthropic-beta")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(okBody))
	})
	out, err := postClaude(context.Background(), []byte(`{"x":1}`))
	if err != nil || string(out) != okBody {
		t.Fatalf("got %q %v", out, err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d", hits.Load())
	}
	if gotKey != "test-key" || gotVersion != claudeAPIVersion || gotType != "application/json" || gotBody != `{"x":1}` {
		t.Errorf("request headers/body wrong: %q %q %q %q", gotKey, gotVersion, gotType, gotBody)
	}
	if gotBeta != "" {
		t.Errorf("no beta header should be sent, got %q", gotBeta)
	}
}

// The original bug: a non-retryable error (400, 401...) lost the API's message
// and surfaced as "read on closed body".
func TestPostClaudeKeepsTheAPIMessageOnClientErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be greater than 0"}}`, "max_tokens: must be greater than 0"},
		{401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, "invalid x-api-key"},
		{404, `not json at all`, "not json at all"},
	} {
		hits, delays := fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := postClaude(context.Background(), []byte(`{}`))
		var apiErr *ClaudeAPIError
		if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
			t.Fatalf("status %d: want ClaudeAPIError, got %v", tc.status, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: message lost: %v", tc.status, err)
		}
		if strings.Contains(err.Error(), "closed") {
			t.Errorf("status %d: closed-body error leaked: %v", tc.status, err)
		}
		if hits.Load() != 1 || len(*delays) != 0 {
			t.Errorf("status %d must not be retried (hits=%d delays=%v)", tc.status, hits.Load(), *delays)
		}
	}
}

func TestPostClaudeRetriesTransientErrorsThenSucceeds(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504, 529} {
		var n atomic.Int32
		hits, delays := fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) < 3 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
				return
			}
			_, _ = w.Write([]byte(okBody))
		})
		out, err := postClaude(context.Background(), []byte(`{}`))
		if err != nil || string(out) != okBody {
			t.Fatalf("status %d: %q %v", status, out, err)
		}
		if hits.Load() != 3 || len(*delays) != 2 {
			t.Errorf("status %d: hits=%d delays=%v", status, hits.Load(), *delays)
		}
		if (*delays)[1] <= (*delays)[0] {
			t.Errorf("status %d: backoff must grow: %v", status, *delays)
		}
	}
}

func TestPostClaudeGivesUpAfterTheLastAttemptWithTheRealReason(t *testing.T) {
	hits, delays := fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"error":{"message":"Overloaded"}}`))
	})
	_, err := postClaude(context.Background(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "Overloaded") || !strings.Contains(err.Error(), "529") {
		t.Fatalf("final error must carry status and message: %v", err)
	}
	var apiErr *ClaudeAPIError
	if !errors.As(err, &apiErr) {
		t.Error("final error must unwrap to ClaudeAPIError")
	}
	if int(hits.Load()) != claudeMaxAttempts || len(*delays) != claudeMaxAttempts-1 {
		t.Errorf("hits=%d delays=%v (no pointless sleep after the last attempt)", hits.Load(), *delays)
	}
}

func TestPostClaudeHonoursRetryAfter(t *testing.T) {
	var n atomic.Int32
	_, delays := fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			return
		}
		_, _ = w.Write([]byte(okBody))
	})
	if _, err := postClaude(context.Background(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(*delays) != 1 || (*delays)[0] != 7*time.Second {
		t.Errorf("delays = %v, want [7s]", *delays)
	}
}

func TestRetryDelayBounds(t *testing.T) {
	if d := retryDelay(0, "9999"); d != claudeMaxRetryDelay {
		t.Errorf("huge Retry-After must be capped: %v", d)
	}
	if d := retryDelay(0, "-3"); d != claudeBaseDelay {
		t.Errorf("negative Retry-After is ignored: %v", d)
	}
	if d := retryDelay(0, "soon"); d != claudeBaseDelay {
		t.Errorf("non-numeric Retry-After is ignored: %v", d)
	}
	if d := retryDelay(10, ""); d != claudeMaxRetryDelay {
		t.Errorf("backoff must be capped: %v", d)
	}
}

func TestPostClaudeRetriesNetworkErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens any more: every attempt fails to connect
	oldURL, oldSleep := claudeURL, claudeSleep
	var sleeps int
	claudeURL = url
	claudeSleep = func(ctx context.Context, d time.Duration) error { sleeps++; return nil }
	defer func() { claudeURL, claudeSleep = oldURL, oldSleep }()
	t.Setenv("ANTHROPIC_API_KEY", "k")
	_, err := postClaude(context.Background(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("got %v", err)
	}
	if sleeps != claudeMaxAttempts-1 {
		t.Errorf("sleeps = %d", sleeps)
	}
}

func TestPostClaudeStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hits, _ := fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
		cancel() // the caller gives up during the first (failing) attempt
		w.WriteHeader(503)
	})
	_, err := postClaude(ctx, []byte(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("must not keep retrying after cancellation: hits=%d", hits.Load())
	}
}

func TestPostClaudeRequiresAnAPIKey(t *testing.T) {
	hits, _ := fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {})
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := postClaude(context.Background(), []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("got %v", err)
	}
	if hits.Load() != 0 {
		t.Error("must not contact the API without a key")
	}
}

func TestCallClaudeWithCacheParsesTheResponseAndSendsCacheControl(t *testing.T) {
	var body string
	fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"listo"}],"usage":{"cache_read_input_tokens":12}}`))
	})
	resp, err := callClaudeWithCache("system rules", "the product", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "listo" || resp.Usage.CacheReadInputTokens != 12 {
		t.Errorf("parsed response: %+v", resp)
	}
	for _, want := range []string{`"cache_control"`, `"ephemeral"`, `system rules`, `the product`, `"max_tokens":100`} {
		if !strings.Contains(body, want) {
			t.Errorf("request body is missing %s: %s", want, body)
		}
	}
}

func TestCallClaudeWithCacheSurfacesAPIErrorsAndBadJSON(t *testing.T) {
	fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"prompt is too long"}}`))
	})
	if _, err := callClaudeWithCache("s", "u", 10); err == nil || !strings.Contains(err.Error(), "prompt is too long") {
		t.Errorf("API message must reach the caller: %v", err)
	}
	fakeClaude(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html>oops</html>`)) })
	if _, err := callClaudeWithCache("s", "u", 10); err == nil || !strings.Contains(err.Error(), "parsing response") {
		t.Errorf("bad JSON: %v", err)
	}
}

// The migrated call sites used to retry every non-200 (including 400/401) and
// throw the API's message away. They now share postClaude.
func TestMigratedCallSitesFailFastAndKeepTheAPIMessage(t *testing.T) {
	calls := map[string]func() error{
		"improveProductNameWithClaude": func() error { _, err := improveProductNameWithClaude("TALADRO VOLTIX 18V"); return err },
		"standardizeWithClaude":        func() error { _, err := standardizeWithClaude("Garantía de 1 año"); return err },
		"generateImageSearchTerms": func() error {
			_, err := generateImageSearchTermsWithClaude(&Product{Name: "Taladro Voltix"})
			return err
		},
		"generateEnrichedImageTerms": func() error {
			_, err := generateEnrichedImageSearchTermsWithClaude(&Product{Name: "Taladro Voltix"})
			return err
		},
	}
	for name, call := range calls {
		hits, delays := fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"credit balance is too low"}}`))
		})
		err := call()
		if err == nil || !strings.Contains(err.Error(), "credit balance is too low") {
			t.Errorf("%s: API message lost: %v", name, err)
		}
		if hits.Load() != 1 || len(*delays) != 0 {
			t.Errorf("%s: a 400 must not be retried (hits=%d delays=%v)", name, hits.Load(), *delays)
		}
	}
}

func TestMigratedCallSitesRecoverFromOverload(t *testing.T) {
	var n atomic.Int32
	hits, _ := fakeClaude(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{"error":{"message":"Overloaded"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"Taladro Percutor Voltix 18 V"}]}`))
	})
	got, err := improveProductNameWithClaude("TALADRO VOLTIX 18V")
	if err != nil || strings.TrimSpace(got) == "" {
		t.Fatalf("got %q %v", got, err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2 (one retry)", hits.Load())
	}
}

func TestMigratedCallSitesRejectEmptyAnswers(t *testing.T) {
	fakeClaude(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"content":[]}`)) })
	if _, err := improveProductNameWithClaude("X"); err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Errorf("got %v", err)
	}
}
