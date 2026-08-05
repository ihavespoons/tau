package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// The device-code flow (RFC 8628) is how a terminal application authenticates
// without ever handling a password: the user is given a short code, types it
// into a browser somewhere, and the application polls until the authorization
// server says they are done.
//
// The polling rules are the fiddly part, and getting them wrong is what makes
// a login fail intermittently rather than never.

const (
	// minPollInterval is a floor on how often to ask. Some servers report an
	// interval of 0, and honouring it literally means a tight loop.
	minPollInterval = time.Second
	// defaultPollInterval is RFC 8628 §3.2's answer when the server omits one.
	defaultPollInterval = 5 * time.Second
	// slowDownIncrement is RFC 8628 §3.5: a slow_down response means add five
	// seconds to the interval.
	slowDownIncrement = 5 * time.Second
)

// ErrLoginCancelled is returned when the caller aborts a device flow.
var ErrLoginCancelled = errors.New("login cancelled")

// validateVerificationURI checks a URI the authorization server asked us to
// open in the user's browser.
//
// This is a security check, not a formatting one. The value comes from an HTTP
// response and is handed to the platform's opener, so an unconstrained scheme
// could name a local executable or a file. Most providers are held to https;
// allowHTTP exists for GitHub, whose enterprise installations are reachable
// over plain http on an internal network.
func validateVerificationURI(provider, raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%s: untrusted verification_uri in device code response", provider)
	}
	if u.Scheme != "https" && (!allowHTTP || u.Scheme != "http") {
		return "", fmt.Errorf("%s: untrusted verification_uri in device code response", provider)
	}
	return u.String(), nil
}

// PollStatus is the outcome of one poll.
type PollStatus int

const (
	// PollPending means the user has not finished yet.
	PollPending PollStatus = iota
	// PollSlowDown means the server wants a longer interval.
	PollSlowDown
	// PollComplete means the flow succeeded.
	PollComplete
	// PollFailed means the flow failed and polling should stop.
	PollFailed
)

// PollResult is what one poll reports.
type PollResult[T any] struct {
	Status PollStatus
	Value  T
	// Message explains a PollFailed.
	Message string
	// Interval is a server-supplied replacement interval, in seconds.
	Interval float64
}

// DeviceCodeOptions configures the poll loop.
type DeviceCodeOptions[T any] struct {
	// IntervalSeconds is the server's requested polling interval.
	IntervalSeconds float64
	// ExpiresInSeconds is how long the device code stays valid. Zero means no
	// deadline, which only makes sense in a test.
	ExpiresInSeconds float64
	// WaitBeforeFirstPoll delays the first attempt by one interval. Some
	// servers reject a poll that arrives before the user could possibly have
	// finished.
	WaitBeforeFirstPoll bool
	// Poll performs one attempt.
	Poll func(ctx context.Context) (PollResult[T], error)
	// Sleep overrides the wait, for tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

// PollDeviceCode runs the poll loop until the flow completes, fails, or the
// device code expires.
func PollDeviceCode[T any](ctx context.Context, opts DeviceCodeOptions[T]) (T, error) {
	var zero T

	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	deadline := time.Time{}
	if opts.ExpiresInSeconds > 0 {
		deadline = time.Now().Add(time.Duration(opts.ExpiresInSeconds * float64(time.Second)))
	}
	remaining := func() time.Duration {
		if deadline.IsZero() {
			return time.Duration(1<<62 - 1)
		}
		return time.Until(deadline)
	}

	interval := clampInterval(opts.IntervalSeconds, defaultPollInterval)
	slowDowns := 0

	if opts.WaitBeforeFirstPoll {
		if wait := min(interval, remaining()); wait > 0 {
			if err := sleep(ctx, wait); err != nil {
				return zero, err
			}
		}
	}

	for remaining() > 0 {
		if ctx.Err() != nil {
			return zero, ErrLoginCancelled
		}

		result, err := opts.Poll(ctx)
		if err != nil {
			return zero, err
		}

		switch result.Status {
		case PollComplete:
			return result.Value, nil
		case PollFailed:
			return zero, errors.New(result.Message)
		case PollSlowDown:
			slowDowns++
			// The server's own number wins when it gives one: GitHub reports
			// the new required minimum there, and a client-tracked value can
			// drift below it forever on a host whose clock is wrong.
			if result.Interval > 0 {
				interval = clampInterval(result.Interval, interval)
			} else {
				interval = max(minPollInterval, interval+slowDownIncrement)
			}
		}

		wait := min(interval, remaining())
		if wait <= 0 {
			break
		}
		if err := sleep(ctx, wait); err != nil {
			return zero, err
		}
	}

	if slowDowns > 0 {
		// Worth distinguishing: repeated slow_down responses followed by a
		// timeout almost always means the host's clock is wrong, which the
		// user would otherwise have no way to guess from "timed out".
		return zero, errors.New(
			"device flow timed out after one or more slow_down responses — " +
				"this is usually clock drift in a VM or WSL; sync the clock and try again")
	}
	return zero, errors.New("device flow timed out")
}

func clampInterval(seconds float64, fallback time.Duration) time.Duration {
	if seconds <= 0 {
		return max(minPollInterval, fallback)
	}
	return max(minPollInterval, time.Duration(seconds*float64(time.Second)))
}

// sleepCtx waits, or returns as soon as the caller gives up.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ErrLoginCancelled
	case <-timer.C:
		return nil
	}
}
