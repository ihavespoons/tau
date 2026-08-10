package server

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"sync"

	"github.com/ihavespoons/tau/session"
)

// Status is where an instance is in its life.
type Status string

const (
	StatusStarting Status = "starting"
	StatusOnline   Status = "online"
	StatusStopping Status = "stopping"
	StatusStopped  Status = "stopped"
	StatusError    Status = "error"
)

// ErrNoInstance is returned for an id the supervisor does not know.
var ErrNoInstance = errors.New("no such instance")

// Record is what a client sees of an instance. It is a snapshot: holding one
// does not keep the process alive, and the status in it may already be stale.
type Record struct {
	ID        string `json:"id"`
	Status    Status `json:"status"`
	Cwd       string `json:"cwd"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"createdAt"`
	// Error is why an instance is in StatusError, empty otherwise.
	Error string `json:"error,omitempty"`
}

// Supervisor owns a set of tau processes, one per working directory.
//
// It is the difference between `tau --mode rpc` and a server: that is one agent
// on one pair of pipes for as long as the caller holds them, this is many, each
// addressable by id and outliving any single client connection.
type Supervisor struct {
	// Spawn builds the process for an instance. Nil runs `tau --mode rpc` in
	// the instance's own working directory, which is what the binary does; a
	// test supplies a stand-in, and an embedder can add flags or a sandbox.
	Spawn func(rec Record) (*exec.Cmd, error)

	mu   sync.Mutex
	live map[string]*supervised
}

type supervised struct {
	rec      Record
	instance *Instance
}

// NewSupervisor returns an empty supervisor.
func NewSupervisor() *Supervisor {
	return &Supervisor{live: map[string]*supervised{}}
}

// defaultSpawn runs the tau on PATH in RPC mode.
func defaultSpawn(rec Record) (*exec.Cmd, error) {
	path, err := exec.LookPath("tau")
	if err != nil {
		return nil, fmt.Errorf("find tau: %w", err)
	}
	cmd := exec.Command(path, "--mode", "rpc")
	cmd.Dir = rec.Cwd
	return cmd, nil
}

// Start launches an instance for a working directory.
//
// It returns as soon as the process is running, not once the agent is ready:
// tau in RPC mode reports readiness on its own stream, and a client that cares
// watches for it rather than having the supervisor guess on its behalf.
func (s *Supervisor) Start(cwd, label string) (Record, error) {
	rec := Record{
		ID:        session.NewID(),
		Status:    StatusStarting,
		Cwd:       cwd,
		Label:     label,
		CreatedAt: session.Now(),
	}

	spawn := s.Spawn
	if spawn == nil {
		spawn = defaultSpawn
	}
	cmd, err := spawn(rec)
	if err != nil {
		rec.Status, rec.Error = StatusError, err.Error()
		return rec, err
	}

	inst, err := Start(cmd)
	if err != nil {
		rec.Status, rec.Error = StatusError, err.Error()
		return rec, err
	}
	rec.Status = StatusOnline

	entry := &supervised{rec: rec, instance: inst}
	s.mu.Lock()
	s.live[rec.ID] = entry
	s.mu.Unlock()

	// A process can die without anyone having asked it to — a crash, an OOM, a
	// user killing it. The record has to follow, or a listing would go on
	// advertising an agent that is not there.
	go s.watch(entry)
	return rec, nil
}

func (s *Supervisor) watch(entry *supervised) {
	<-entry.instance.Done()

	s.mu.Lock()
	defer s.mu.Unlock()
	// Stopping means someone asked, and that is not a failure however the
	// process ended up exiting.
	if entry.rec.Status == StatusStopping {
		entry.rec.Status = StatusStopped
		return
	}
	if err := entry.instance.Err(); err != nil {
		entry.rec.Status, entry.rec.Error = StatusError, err.Error()
		return
	}
	entry.rec.Status = StatusStopped
}

// List returns every instance the supervisor knows, newest first.
//
// Stopped and failed ones are included. A client asking what happened to an
// agent that died needs it to still be in the answer.
func (s *Supervisor) List() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Record, 0, len(s.live))
	for _, e := range s.live {
		out = append(out, e.rec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Get returns one instance's record.
func (s *Supervisor) Get(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live[id]
	if !ok {
		return Record{}, false
	}
	return e.rec, true
}

// Instance returns the live process for an id, for sending commands and
// subscribing to its stream.
func (s *Supervisor) Instance(id string) (*Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoInstance, id)
	}
	return e.instance, nil
}

// Stop shuts an instance down and leaves its record behind.
func (s *Supervisor) Stop(ctx context.Context, id string) (Record, error) {
	s.mu.Lock()
	e, ok := s.live[id]
	if !ok {
		s.mu.Unlock()
		return Record{}, fmt.Errorf("%w: %s", ErrNoInstance, id)
	}
	// Marked before the wait so watch knows this exit was asked for, whichever
	// of the two goroutines gets there first.
	if e.rec.Status == StatusOnline || e.rec.Status == StatusStarting {
		e.rec.Status = StatusStopping
	}
	s.mu.Unlock()

	err := e.instance.Close(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	return e.rec, err
}

// Forget drops a stopped instance's record. A running one is left alone: losing
// track of a live process would leak it.
func (s *Supervisor) Forget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoInstance, id)
	}
	if e.rec.Status == StatusOnline || e.rec.Status == StatusStarting || e.rec.Status == StatusStopping {
		return fmt.Errorf("instance %s is still running", id)
	}
	delete(s.live, id)
	return nil
}

// Shutdown stops every instance. It always tries all of them, so one process
// refusing to go does not strand the rest.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.live))
	for id := range s.live {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	var errs []error
	for _, id := range ids {
		if _, err := s.Stop(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
