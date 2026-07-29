package oauth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// recordSleeps replaces the wait so the loop runs instantly and every interval
// the implementation chose can be asserted.
func recordSleeps(waits *[]time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
}

func TestPollsUntilComplete(t *testing.T) {
	attempts := 0
	var waits []time.Duration

	got, err := PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		IntervalSeconds:  5,
		ExpiresInSeconds: 900,
		Sleep:            recordSleeps(&waits),
		Poll: func(context.Context) (PollResult[string], error) {
			attempts++
			if attempts < 3 {
				return PollResult[string]{Status: PollPending}, nil
			}
			return PollResult[string]{Status: PollComplete, Value: "token"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "token" {
		t.Errorf("value: %q", got)
	}
	if attempts != 3 {
		t.Errorf("attempts: %d", attempts)
	}
	for _, w := range waits {
		if w != 5*time.Second {
			t.Errorf("wait: %v, want the server's interval", w)
		}
	}
}

// RFC 8628 §3.2: with no interval given, five seconds.
func TestDefaultInterval(t *testing.T) {
	var waits []time.Duration
	_, _ = PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		ExpiresInSeconds: 900,
		Sleep:            recordSleeps(&waits),
		Poll: func(context.Context) (PollResult[string], error) {
			if len(waits) > 0 {
				return PollResult[string]{Status: PollComplete}, nil
			}
			return PollResult[string]{Status: PollPending}, nil
		},
	})

	if len(waits) == 0 || waits[0] != defaultPollInterval {
		t.Errorf("waits: %v, want the RFC default first", waits)
	}
}

// THE POINT: some servers report an interval of 0, and honouring it literally
// is a tight loop that hammers the endpoint until it rate-limits the user out
// of their own login.
func TestIntervalHasAFloor(t *testing.T) {
	var waits []time.Duration
	_, _ = PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		IntervalSeconds:  0,
		ExpiresInSeconds: 900,
		Sleep:            recordSleeps(&waits),
		Poll: func(context.Context) (PollResult[string], error) {
			if len(waits) > 0 {
				return PollResult[string]{Status: PollComplete}, nil
			}
			return PollResult[string]{Status: PollPending}, nil
		},
	})

	if len(waits) == 0 || waits[0] < minPollInterval {
		t.Errorf("waits: %v, want at least %v", waits, minPollInterval)
	}
}

// RFC 8628 §3.5: slow_down means add five seconds.
func TestSlowDownIncreasesTheInterval(t *testing.T) {
	var waits []time.Duration
	attempts := 0

	_, _ = PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		IntervalSeconds:  5,
		ExpiresInSeconds: 900,
		Sleep:            recordSleeps(&waits),
		Poll: func(context.Context) (PollResult[string], error) {
			attempts++
			switch attempts {
			case 1:
				return PollResult[string]{Status: PollSlowDown}, nil
			case 2:
				return PollResult[string]{Status: PollSlowDown}, nil
			default:
				return PollResult[string]{Status: PollComplete}, nil
			}
		},
	})

	want := []time.Duration{10 * time.Second, 15 * time.Second}
	for i, w := range want {
		if i >= len(waits) || waits[i] != w {
			t.Fatalf("waits: %v, want %v", waits, want)
		}
	}
}

// THE POINT: when the server names a new interval, that number wins. GitHub
// reports the new required minimum there, and a client-tracked value can drift
// below it forever on a host whose clock is wrong — which is exactly the case
// this rule exists for.
func TestServerSuppliedIntervalWins(t *testing.T) {
	var waits []time.Duration
	attempts := 0

	_, _ = PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		IntervalSeconds:  5,
		ExpiresInSeconds: 900,
		Sleep:            recordSleeps(&waits),
		Poll: func(context.Context) (PollResult[string], error) {
			attempts++
			if attempts == 1 {
				return PollResult[string]{Status: PollSlowDown, Interval: 30}, nil
			}
			return PollResult[string]{Status: PollComplete}, nil
		},
	})

	if len(waits) == 0 || waits[0] != 30*time.Second {
		t.Errorf("waits: %v, want the server's 30s", waits)
	}
}

// A failure stops the loop immediately: continuing to poll after the server
// says the request was denied only wastes the user's time.
func TestFailureStopsPolling(t *testing.T) {
	attempts := 0
	_, err := PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		ExpiresInSeconds: 900,
		Sleep:            func(context.Context, time.Duration) error { return nil },
		Poll: func(context.Context) (PollResult[string], error) {
			attempts++
			return PollResult[string]{Status: PollFailed, Message: "access_denied"}, nil
		},
	})

	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error: %v", err)
	}
	if attempts != 1 {
		t.Errorf("polled %d times after a failure", attempts)
	}
}

// A timeout after slow_down responses almost always means the host's clock is
// wrong, which the user has no way to guess from "timed out" alone.
func TestSlowDownTimeoutExplainsClockDrift(t *testing.T) {
	_, err := PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		IntervalSeconds:  1,
		ExpiresInSeconds: 0.001,
		Sleep:            func(context.Context, time.Duration) error { return nil },
		Poll: func(context.Context) (PollResult[string], error) {
			return PollResult[string]{Status: PollSlowDown}, nil
		},
	})

	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), "clock") {
		t.Errorf("the error should point at clock drift: %v", err)
	}
}

// A plain timeout says so, without the clock advice that would be a red
// herring.
func TestPlainTimeout(t *testing.T) {
	_, err := PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		IntervalSeconds:  1,
		ExpiresInSeconds: 0.001,
		Sleep:            func(context.Context, time.Duration) error { return nil },
		Poll: func(context.Context) (PollResult[string], error) {
			return PollResult[string]{Status: PollPending}, nil
		},
	})

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error: %v", err)
	}
	if strings.Contains(err.Error(), "clock") {
		t.Error("clock advice on a plain timeout is a red herring")
	}
}

// Cancelling has to stop the loop promptly — a user who pressed Ctrl+C should
// not wait out a thirty-second interval.
func TestCancellationStopsTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	_, err := PollDeviceCode(ctx, DeviceCodeOptions[string]{
		IntervalSeconds:  30,
		ExpiresInSeconds: 900,
		Poll: func(context.Context) (PollResult[string], error) {
			cancel()
			return PollResult[string]{Status: PollPending}, nil
		},
	})

	if !errors.Is(err, ErrLoginCancelled) {
		t.Errorf("error: %v", err)
	}
}

// Some servers reject a poll that arrives before the user could possibly have
// finished typing the code.
func TestWaitBeforeFirstPoll(t *testing.T) {
	var order []string
	_, _ = PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		IntervalSeconds:     5,
		ExpiresInSeconds:    900,
		WaitBeforeFirstPoll: true,
		Sleep: func(context.Context, time.Duration) error {
			order = append(order, "sleep")
			return nil
		},
		Poll: func(context.Context) (PollResult[string], error) {
			order = append(order, "poll")
			return PollResult[string]{Status: PollComplete}, nil
		},
	})

	if len(order) < 2 || order[0] != "sleep" || order[1] != "poll" {
		t.Errorf("order: %v, want a wait before the first poll", order)
	}
}

// A transport error is not a poll result — it means the request never
// completed, and retrying blindly would hide a broken network.
func TestTransportErrorsPropagate(t *testing.T) {
	sentinel := errors.New("connection refused")
	_, err := PollDeviceCode(context.Background(), DeviceCodeOptions[string]{
		ExpiresInSeconds: 900,
		Sleep:            func(context.Context, time.Duration) error { return nil },
		Poll: func(context.Context) (PollResult[string], error) {
			return PollResult[string]{}, sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("error: %v", err)
	}
}
