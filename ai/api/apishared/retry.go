package apishared

// Port of Pi's utils/provider-retry.ts: SDK-equivalent retry behavior with an
// interruptible backoff sleep and a cap on server-requested delays.
//
// This lived in the anthropic package until the OpenAI wires needed it too.
// Pi applies the same policy on every wire, and a second copy would drift into
// two different answers to "is this failure worth retrying" — which is the
// question that decides whether a 429 ends a session or costs three seconds.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultMaxRetryDelayMs = 60_000

// httpError is a non-2xx provider response (Pi's ProviderError analog).
type httpError struct {
	status int
	header http.Header
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// newHTTPError drains up to 64 KiB of the body for the error message.
func newHTTPError(resp *http.Response) *httpError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	_ = resp.Body.Close()
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	} else {
		msg = fmt.Sprintf("%d %s", resp.StatusCode, msg)
	}
	return &httpError{status: resp.StatusCode, header: resp.Header, msg: msg}
}

func isRetryable(err error) bool {
	var he *httpError
	if !errors.As(err, &he) {
		// Connection-level failures (no HTTP status) are retryable,
		// matching the SDK policy Pi mirrors.
		return true
	}
	switch he.header.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	return he.status == 408 || he.status == 409 || he.status == 429 || he.status >= 500
}

func validateServerRetryDelay(delayMs float64, maxRetryDelayMs int, providerMsg string) (float64, error) {
	maxDelay := float64(defaultMaxRetryDelayMs)
	if maxRetryDelayMs > 0 {
		maxDelay = float64(maxRetryDelayMs)
	} else if maxRetryDelayMs < 0 {
		maxDelay = 0 // cap disabled
	}
	if maxDelay > 0 && delayMs > maxDelay {
		//nolint:staticcheck // Pi error-message parity
		return 0, fmt.Errorf("Server requested %ds retry delay (max: %ds). %s",
			int(math.Ceil(delayMs/1000)), int(math.Ceil(maxDelay/1000)), providerMsg)
	}
	return delayMs, nil
}

func retryDelayMs(err error, retryIndex int, maxRetryDelayMs int) (float64, error) {
	var he *httpError
	if errors.As(err, &he) {
		if v := he.header.Get("retry-after-ms"); v != "" {
			if ms, perr := strconv.ParseFloat(v, 64); perr == nil {
				return validateServerRetryDelay(ms, maxRetryDelayMs, he.msg)
			}
		}
		if v := he.header.Get("retry-after"); v != "" {
			if secs, perr := strconv.ParseFloat(v, 64); perr == nil {
				return validateServerRetryDelay(secs*1000, maxRetryDelayMs, he.msg)
			}
			if t, perr := http.ParseTime(v); perr == nil {
				return validateServerRetryDelay(float64(time.Until(t).Milliseconds()), maxRetryDelayMs, he.msg)
			}
		}
	}
	exponential := math.Min(0.5*math.Pow(2, float64(retryIndex)), 8) * 1000
	return exponential * (1 - rand.Float64()*0.25), nil
}

// RetryRequest runs do with Pi's retry policy. maxRetries nil means 0.
func RetryRequest(ctx context.Context, do func() (*http.Response, error), maxRetries *int, maxRetryDelayMs int) (*http.Response, error) {
	retries := 0
	if maxRetries != nil {
		retries = *maxRetries
	}
	remaining := retries
	for {
		resp, err := do()
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		if err == nil {
			err = newHTTPError(resp)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if remaining <= 0 || !isRetryable(err) {
			return nil, err
		}
		retryIndex := retries - remaining
		remaining--
		delayMs, derr := retryDelayMs(err, retryIndex, maxRetryDelayMs)
		if derr != nil {
			return nil, derr
		}
		select {
		case <-time.After(time.Duration(delayMs) * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
