package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Shared transport for calls to the Claude Messages API: one reusable client,
// bounded retries with backoff, Retry-After support, and an error that always
// carries the API's own message.

const (
	claudeMessagesURL   = "https://api.anthropic.com/v1/messages"
	claudeAPIVersion    = "2023-06-01"
	claudeMaxAttempts   = 3
	claudeMaxRetryDelay = 30 * time.Second
	claudeMaxBodyBytes  = 8 << 20 // a response is JSON text; 8 MB is far above any real one
)

// Package-level so tests can point it at a local server and shorten delays.
var (
	claudeURL        = claudeMessagesURL
	claudeHTTPClient = &http.Client{Timeout: 60 * time.Second}
	claudeBaseDelay  = 1 * time.Second
	claudeSleep      = sleepCtx
)

// ClaudeAPIError is a non-success answer from the API.
type ClaudeAPIError struct {
	Status  int
	Message string
}

func (e *ClaudeAPIError) Error() string {
	return fmt.Sprintf("Claude API status %d: %s", e.Status, e.Message)
}

// retryable reports whether the same request may succeed if sent again.
func (e *ClaudeAPIError) retryable() bool {
	switch e.Status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529: // 529 = overloaded
		return true
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryDelay is the wait before attempt number `attempt` (0-based) is retried:
// the server's Retry-After when it sends one, else exponential backoff.
func retryDelay(attempt int, retryAfter string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		d := time.Duration(s) * time.Second
		if d > claudeMaxRetryDelay {
			d = claudeMaxRetryDelay
		}
		return d
	}
	d := claudeBaseDelay << attempt
	if d > claudeMaxRetryDelay {
		d = claudeMaxRetryDelay
	}
	return d
}

// apiErrorMessage extracts error.message from the API's JSON error, or falls
// back to the (truncated) raw body.
func apiErrorMessage(body []byte) string {
	s := string(body)
	if i := strings.Index(s, `"message":"`); i >= 0 {
		rest := s[i+len(`"message":"`):]
		if j := strings.Index(rest, `"`); j > 0 {
			return rest[:j]
		}
	}
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// postClaude sends one Messages API request and returns the response body.
// It retries network errors, 429, 5xx and 529, never retries other 4xx, and
// closes every response body exactly once.
func postClaude(ctx context.Context, requestBody []byte) ([]byte, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, errors.New("ANTHROPIC_API_KEY not configured")
	}
	var lastErr error
	for attempt := 0; attempt < claudeMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeURL, bytes.NewReader(requestBody))
		if err != nil {
			return nil, fmt.Errorf("error creating request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", claudeAPIVersion)

		wait := retryDelay(attempt, "")
		resp, err := claudeHTTPClient.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		} else {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, claudeMaxBodyBytes))
			resp.Body.Close()
			switch {
			case readErr != nil:
				lastErr = fmt.Errorf("error reading response: %w", readErr)
			case resp.StatusCode == http.StatusOK:
				return body, nil
			default:
				apiErr := &ClaudeAPIError{Status: resp.StatusCode, Message: apiErrorMessage(body)}
				if !apiErr.retryable() {
					return nil, apiErr
				}
				lastErr = apiErr
				wait = retryDelay(attempt, resp.Header.Get("Retry-After"))
			}
		}
		if attempt == claudeMaxAttempts-1 {
			break
		}
		if err := claudeSleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("claude request failed after %d attempts: %w", claudeMaxAttempts, lastErr)
}
