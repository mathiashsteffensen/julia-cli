package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

const (
	rateLimitAttempts = 5
	rateLimitBudget   = 2 * time.Minute
	rateLimitDelay    = 5 * time.Second
)

// Google drops response headers when it converts an HTTP error. Capture only
// Retry-After on the request's own context, without sharing state across calls.
type retryHeadersKey struct{}
type retryHeaders struct{ after, afterMS string }

type rateLimitTransport struct{ base http.RoundTripper }

func (t rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if response != nil && response.StatusCode == http.StatusTooManyRequests {
		if h, ok := req.Context().Value(retryHeadersKey{}).(*retryHeaders); ok {
			h.after = response.Header.Get("Retry-After")
			h.afterMS = response.Header.Get("Retry-After-Ms")
		}
	}
	return response, err
}

// rateLimitModel retries only a rejected model request before it has emitted
// content. Fantasy's normal non-429 retry policy remains in place. One wrapper
// is made per agent step, so neither a prior tool execution nor a partial
// response is replayed for a rate limit.
type rateLimitModel struct {
	fantasy.LanguageModel
	onRetry func(attempt int, delay time.Duration)

	attempts    int
	started     time.Time
	rateLimited bool
	lastError   error
}

// rateLimitError deliberately hides the provider error from Fantasy, whose
// own retry loop would otherwise multiply the 429 attempts or replay output.
type rateLimitError struct{ message string }

func (e rateLimitError) Error() string { return e.message }

func (m *rateLimitModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		for {
			if ctx.Err() != nil {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
				return
			}
			if m.started.IsZero() {
				m.started = time.Now()
			}
			// Fantasy may retry a different error after a 429. Keep those
			// retries inside the same attempt and time budget as well.
			if m.rateLimited && (m.attempts >= rateLimitAttempts || time.Since(m.started) >= rateLimitBudget) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: rateLimitError{message: fmt.Sprintf("%v (rate limit retry budget exhausted)", m.lastError)}})
				return
			}
			m.attempts++
			headers := &retryHeaders{}
			requestCtx := context.WithValue(ctx, retryHeadersKey{}, headers)
			stream, err := m.LanguageModel.Stream(requestCtx, call)
			emitted := false
			var warnings []fantasy.StreamPart
			if err == nil {
				stream(func(part fantasy.StreamPart) bool {
					if part.Type == fantasy.StreamPartTypeError {
						err = part.Error
						return false
					}
					if part.Type == fantasy.StreamPartTypeWarnings && !emitted {
						warnings = append(warnings, part)
						return true
					}
					for _, warning := range warnings {
						if !yield(warning) {
							return false
						}
					}
					warnings = nil
					emitted = true
					return yield(part)
				})
			}
			if err == nil {
				for _, warning := range warnings {
					if !yield(warning) {
						return
					}
				}
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
				return
			}
			m.lastError = err
			var providerErr *fantasy.ProviderError
			if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusTooManyRequests {
				if m.rateLimited && m.attempts >= rateLimitAttempts {
					err = rateLimitError{message: fmt.Sprintf("%v (rate limit retry budget exhausted)", err)}
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
				return
			}
			m.rateLimited = true

			// A rejected HTTP request has no output; once any stream part has
			// escaped, replaying it could duplicate output or server-side tools.
			delay := retryDelay(providerErr.ResponseHeaders, *headers, m.attempts, time.Now())
			remaining := rateLimitBudget - time.Since(m.started)
			if emitted || m.attempts >= rateLimitAttempts || delay >= remaining {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: rateLimitError{message: fmt.Sprintf("%v (rate limit retry stopped after %d attempt(s))", err, m.attempts)}})
				return
			}
			if m.onRetry != nil {
				m.onRetry(m.attempts+1, delay)
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
				return
			case <-timer.C:
			}
			if time.Since(m.started) >= rateLimitBudget {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: rateLimitError{message: fmt.Sprintf("%v (rate limit retry budget exhausted)", err)}})
				return
			}
		}
	}, nil
}

// retryDelay uses a valid server delay as supplied, even when it is longer
// than the remaining budget; the caller then stops rather than retrying early.
func retryDelay(headers map[string]string, captured retryHeaders, attempt int, now time.Time) time.Duration {
	after, afterMS := captured.after, captured.afterMS
	for name, value := range headers {
		switch {
		case strings.EqualFold(name, "retry-after"):
			after = value
		case strings.EqualFold(name, "retry-after-ms"):
			afterMS = value
		}
	}
	if delay, ok := retrySeconds(afterMS, time.Millisecond); ok {
		return delay
	}
	if delay, ok := retrySeconds(after, time.Second); ok {
		return delay
	}
	if date, err := http.ParseTime(after); err == nil {
		return max(0, date.Sub(now))
	}
	return rateLimitDelay * time.Duration(1<<min(max(attempt-1, 0), 4))
}

func retrySeconds(value string, unit time.Duration) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if math.IsInf(n, 1) || n >= float64(math.MaxInt64)/float64(unit) {
		return time.Duration(math.MaxInt64), true
	}
	if err != nil || math.IsNaN(n) || n < 0 {
		return 0, false
	}
	return time.Duration(n * float64(unit)), true
}
