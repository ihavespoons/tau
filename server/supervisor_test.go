package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ihavespoons/tau/rpc"
)

func testSupervisor(t *testing.T, mode string) *Supervisor {
	t.Helper()
	s := NewSupervisor()
	s.Spawn = func(Record) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(), "TAU_SERVER_TEST_HELPER="+mode)
		return cmd, nil
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

// waitStatus polls until an instance reaches a status, because a process
// exiting and the record following it are two different goroutines.
func waitStatus(t *testing.T, s *Supervisor, id string, want Status) Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := s.Get(id); ok && rec.Status == want {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, _ := s.Get(id)
	t.Fatalf("instance stayed at %q, want %q", rec.Status, want)
	return Record{}
}

func TestStartingAnInstanceMakesItListable(t *testing.T) {
	s := testSupervisor(t, "echo")

	rec, err := s.Start("/work", "the label")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusOnline {
		t.Errorf("status = %q, want online", rec.Status)
	}
	if rec.ID == "" || rec.CreatedAt == "" {
		t.Errorf("record is missing identity: %+v", rec)
	}

	listed := s.List()
	if len(listed) != 1 || listed[0].ID != rec.ID {
		t.Fatalf("list = %+v, want the started instance", listed)
	}
	if listed[0].Cwd != "/work" || listed[0].Label != "the label" {
		t.Errorf("record lost what it was started with: %+v", listed[0])
	}
}

func TestAnUnknownIdIsAnError(t *testing.T) {
	s := testSupervisor(t, "echo")

	if _, ok := s.Get("nope"); ok {
		t.Error("an unknown id was found")
	}
	if _, err := s.Instance("nope"); !errors.Is(err, ErrNoInstance) {
		t.Errorf("err = %v, want no-such-instance", err)
	}
	if _, err := s.Stop(context.Background(), "nope"); !errors.Is(err, ErrNoInstance) {
		t.Errorf("err = %v, want no-such-instance", err)
	}
}

// The supervisor's job is addressing agents by id, so a command has to reach
// the right one.
func TestACommandReachesTheNamedInstance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := testSupervisor(t, "echo")

	rec, err := s.Start("/work", "")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := s.Instance(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := inst.Do(ctx, rpc.Command{ID: "c1", Type: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "c1" {
		t.Errorf("response = %+v", res)
	}
}

func TestStoppingLeavesTheRecordBehind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := testSupervisor(t, "echo")

	rec, err := s.Start("/work", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}

	stopped := waitStatus(t, s, rec.ID, StatusStopped)
	if stopped.Error != "" {
		t.Errorf("a requested stop was recorded as a failure: %q", stopped.Error)
	}
	// A client asking what happened to an agent needs it still in the answer.
	if len(s.List()) != 1 {
		t.Errorf("the record vanished on stop: %+v", s.List())
	}
}

// A process nobody asked to stop has to show up as failed, or a listing would
// go on advertising an agent that is not there.
func TestACrashBecomesAnError(t *testing.T) {
	s := testSupervisor(t, "crash")

	rec, err := s.Start("/work", "")
	if err != nil {
		t.Fatal(err)
	}
	failed := waitStatus(t, s, rec.ID, StatusError)
	if failed.Error == "" {
		t.Error("a crashed instance recorded no reason")
	}
}

// Exiting cleanly without being asked is not a failure.
func TestACleanExitIsNotAnError(t *testing.T) {
	s := testSupervisor(t, "events")

	rec, err := s.Start("/work", "")
	if err != nil {
		t.Fatal(err)
	}
	stopped := waitStatus(t, s, rec.ID, StatusStopped)
	if stopped.Error != "" {
		t.Errorf("a clean exit recorded an error: %q", stopped.Error)
	}
}

func TestForgettingIsOnlyForStoppedInstances(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := testSupervisor(t, "echo")

	rec, err := s.Start("/work", "")
	if err != nil {
		t.Fatal(err)
	}
	// Losing track of a live process would leak it.
	if err := s.Forget(rec.ID); err == nil {
		t.Error("a running instance was forgotten")
	}

	if _, err := s.Stop(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, s, rec.ID, StatusStopped)

	if err := s.Forget(rec.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Errorf("the record survived being forgotten: %+v", s.List())
	}
}

func TestShutdownStopsEverything(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := testSupervisor(t, "echo")

	var ids []string
	for range 3 {
		rec, err := s.Start("/work", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}

	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		waitStatus(t, s, id, StatusStopped)
	}
}

// A spawn that cannot even build a command must report it rather than leaving a
// half-made record listed as online.
func TestAFailedSpawnIsRecordedAsAnError(t *testing.T) {
	s := NewSupervisor()
	s.Spawn = func(Record) (*exec.Cmd, error) {
		return nil, errors.New("no tau on PATH")
	}

	rec, err := s.Start("/work", "")
	if err == nil {
		t.Fatal("a failed spawn reported success")
	}
	if rec.Status != StatusError || rec.Error == "" {
		t.Errorf("record = %+v, want an error status with a reason", rec)
	}
	// And nothing is left listed, because nothing was started.
	if len(s.List()) != 0 {
		t.Errorf("a failed spawn left %+v listed", s.List())
	}
}
