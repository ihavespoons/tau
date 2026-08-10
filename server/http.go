package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/ihavespoons/tau/rpc"
)

// Handler is the HTTP API over a supervisor.
//
// HTTP rather than Pi's Unix-socket IPC, because the thing on the other end is
// usually an editor plugin or a deployment's health check rather than another
// copy of the same program, and both of those already speak HTTP. Nothing stops
// it being served on a Unix socket — net/http does not care what it listens on,
// and Serve defaults to one.
func Handler(s *Supervisor) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /instances", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"instances": s.List()})
	})

	mux.HandleFunc("POST /instances", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Cwd   string `json:"cwd"`
			Label string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if body.Cwd == "" {
			writeError(w, http.StatusBadRequest, errors.New("cwd is required"))
			return
		}
		rec, err := s.Start(body.Cwd, body.Label)
		if err != nil {
			// The record is still worth returning: it says why.
			writeJSON(w, http.StatusInternalServerError, rec)
			return
		}
		writeJSON(w, http.StatusCreated, rec)
	})

	mux.HandleFunc("GET /instances/{id}", func(w http.ResponseWriter, r *http.Request) {
		rec, ok := s.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, ErrNoInstance)
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})

	mux.HandleFunc("DELETE /instances/{id}", func(w http.ResponseWriter, r *http.Request) {
		rec, err := s.Stop(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})

	mux.HandleFunc("POST /instances/{id}/rpc", func(w http.ResponseWriter, r *http.Request) {
		inst, err := s.Instance(r.PathValue("id"))
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		var cmd rpc.Command
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		res, err := inst.Do(r.Context(), cmd)
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("GET /instances/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		inst, err := s.Instance(r.PathValue("id"))
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		streamEvents(w, r, inst)
	})

	return mux
}

// streamEvents forwards an instance's stream as server-sent events.
//
// The subscription is taken before anything is written, so an event produced
// while the headers are going out is not lost.
func streamEvents(w http.ResponseWriter, r *http.Request, inst *Instance) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is not supported by this server"))
		return
	}

	lines, stop := inst.Subscribe()
	defer stop()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Without this a reverse proxy will happily buffer the whole stream and
	// deliver it when the turn is over, which is the opposite of the point.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// The process is gone. Saying so is more useful than the
				// connection simply ending, which a client cannot tell from a
				// network fault.
				_, _ = fmt.Fprint(w, "event: instance_gone\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			// The payload is already a single line of JSON — the framing tau
			// writes with — so it needs no re-encoding to be a valid SSE data
			// field.
			//
			// A write that fails means the client is gone. Returning drops the
			// subscription through the deferred stop; carrying on would spend
			// the rest of the turn writing into a closed socket.
			if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNoInstance):
		return http.StatusNotFound
	case errors.Is(err, ErrInstanceGone):
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
