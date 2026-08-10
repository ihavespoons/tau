package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, mode string) (*httptest.Server, *Supervisor) {
	t.Helper()
	sup := testSupervisor(t, mode)
	srv := httptest.NewServer(Handler(sup))
	t.Cleanup(srv.Close)
	return srv, sup
}

func post(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Post(srv.URL+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func decode[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	var out T
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return out
}

func TestStartingAnInstanceOverHTTP(t *testing.T) {
	srv, _ := testServer(t, "echo")

	res := post(t, srv, "/instances", map[string]string{"cwd": "/work", "label": "editor"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	rec := decode[Record](t, res)
	if rec.ID == "" || rec.Status != StatusOnline {
		t.Fatalf("record = %+v", rec)
	}

	listRes, err := srv.Client().Get(srv.URL + "/instances")
	if err != nil {
		t.Fatal(err)
	}
	listed := decode[struct {
		Instances []Record `json:"instances"`
	}](t, listRes)
	if len(listed.Instances) != 1 || listed.Instances[0].ID != rec.ID {
		t.Errorf("list = %+v", listed.Instances)
	}
}

func TestStartingWithoutACwdIsRejected(t *testing.T) {
	srv, _ := testServer(t, "echo")

	res := post(t, srv, "/instances", map[string]string{"label": "nowhere"})
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestAnUnknownInstanceIs404(t *testing.T) {
	srv, _ := testServer(t, "echo")

	res, err := srv.Client().Get(srv.URL + "/instances/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestAnRpcRoundTripOverHTTP(t *testing.T) {
	srv, _ := testServer(t, "echo")

	started := decode[Record](t, post(t, srv, "/instances", map[string]string{"cwd": "/work"}))

	res := post(t, srv, "/instances/"+started.ID+"/rpc",
		map[string]string{"id": "c1", "type": "prompt"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	reply := decode[struct {
		ID      string `json:"id"`
		Success bool   `json:"success"`
	}](t, res)
	if reply.ID != "c1" || !reply.Success {
		t.Errorf("reply = %+v", reply)
	}
}

// A client talking to an agent that has died should be told that, rather than
// getting a generic failure it cannot act on.
func TestTalkingToADeadInstanceIsGone(t *testing.T) {
	srv, sup := testServer(t, "echo")

	started := decode[Record](t, post(t, srv, "/instances", map[string]string{"cwd": "/work"}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sup.Stop(ctx, started.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, sup, started.ID, StatusStopped)

	res := post(t, srv, "/instances/"+started.ID+"/rpc",
		map[string]string{"id": "c1", "type": "prompt"})
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusGone {
		t.Errorf("status = %d, want 410", res.StatusCode)
	}
}

func TestDeletingAnInstanceStopsIt(t *testing.T) {
	srv, sup := testServer(t, "echo")

	started := decode[Record](t, post(t, srv, "/instances", map[string]string{"cwd": "/work"}))

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/instances/"+started.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	waitStatus(t, sup, started.ID, StatusStopped)
}

// The stream is what makes the server usable for a running turn, so an event
// produced by the agent has to reach an HTTP client.
func TestEventsStreamOverSSE(t *testing.T) {
	srv, _ := testServer(t, "echo")

	started := decode[Record](t, post(t, srv, "/instances", map[string]string{"cwd": "/work"}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/instances/"+started.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()

	if got := stream.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}

	// The subscription is live by the time the headers arrived, so a command
	// sent now produces an event the stream will carry.
	res := post(t, srv, "/instances/"+started.ID+"/rpc",
		map[string]string{"id": "c1", "type": "prompt"})
	_ = res.Body.Close()

	lines := bufio.NewScanner(stream.Body)
	for lines.Scan() {
		line := lines.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if strings.Contains(line, "agent_event") {
			return
		}
		if strings.Contains(line, `"type":"response"`) {
			t.Fatal("a correlated response leaked into the event stream")
		}
	}
	t.Fatal("no event arrived on the stream")
}

// A stream on an instance that dies should say so rather than just ending,
// which a client cannot tell from a dropped connection.
func TestTheStreamSaysWhenTheInstanceGoes(t *testing.T) {
	srv, sup := testServer(t, "echo")

	started := decode[Record](t, post(t, srv, "/instances", map[string]string{"cwd": "/work"}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/instances/"+started.ID+"/events", nil)
	stream, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if _, err := sup.Stop(stopCtx, started.ID); err != nil {
		t.Fatal(err)
	}

	lines := bufio.NewScanner(stream.Body)
	for lines.Scan() {
		if strings.Contains(lines.Text(), "instance_gone") {
			return
		}
	}
	t.Error("the stream ended without saying the instance was gone")
}
