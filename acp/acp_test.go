package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/config"
)

// drain reads whatever the connection writes into a channel.
//
// io.Pipe is synchronous: a write blocks until someone reads it, so a test that
// only reads after triggering a write would deadlock on the first one. The
// background scanner is what keeps the writer moving.
func drain(r io.Reader) <-chan Message {
	out := make(chan Message, 64)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			var msg Message
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
				continue
			}
			out <- msg
		}
	}()
	return out
}

// pipeConn wires a Conn to a pair of pipes.
func pipeConn(t *testing.T, h Handler) (in *io.PipeWriter, out <-chan Message, c *Conn) {
	t.Helper()
	serverR, clientW := io.Pipe()
	clientR, serverW := io.Pipe()

	c = NewConn(serverR, serverW, h)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = clientW.Close()
		_ = serverW.Close()
	})
	return clientW, drain(clientR), c
}

func send(t *testing.T, w io.Writer, raw string) {
	t.Helper()
	if _, err := io.WriteString(w, raw+"\n"); err != nil {
		t.Fatal(err)
	}
}

func nextMessage(t *testing.T, out <-chan Message) Message {
	t.Helper()
	select {
	case msg, ok := <-out:
		if !ok {
			t.Fatal("the connection closed without writing")
		}
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("the connection wrote nothing")
		return Message{}
	}
}

func TestARequestGetsAResponseWithItsID(t *testing.T) {
	w, out, _ := pipeConn(t, func(context.Context, string, json.RawMessage) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	})

	send(t, w, `{"jsonrpc":"2.0","id":"a1","method":"anything"}`)
	msg := nextMessage(t, out)

	if string(msg.ID) != `"a1"` {
		t.Errorf("id = %s, want a1", msg.ID)
	}
	if msg.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q", msg.JSONRPC)
	}
	if !strings.Contains(string(msg.Result), `"ok":"yes"`) {
		t.Errorf("result = %s", msg.Result)
	}
}

// A notification has no id, so answering it would send the client a reply it
// is not waiting for.
func TestANotificationIsNotAnswered(t *testing.T) {
	seen := make(chan string, 1)
	w, out, _ := pipeConn(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		seen <- method
		return nil, nil
	})

	send(t, w, `{"jsonrpc":"2.0","method":"session/cancel","params":{}}`)
	select {
	case method := <-seen:
		if method != "session/cancel" {
			t.Errorf("handled %q", method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notification was never handled")
	}

	// Anything written now must be the response to the request that follows,
	// not a reply to the notification.
	send(t, w, `{"jsonrpc":"2.0","id":"a1","method":"ping"}`)
	if got := nextMessage(t, out); string(got.ID) != `"a1"` {
		t.Errorf("first reply had id %s, so the notification was answered", got.ID)
	}
}

func TestAHandlerErrorBecomesAnErrorResponse(t *testing.T) {
	w, out, _ := pipeConn(t, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "cwd is required"}
	})

	send(t, w, `{"jsonrpc":"2.0","id":"a1","method":"session/new"}`)
	msg := nextMessage(t, out)

	if msg.Error == nil {
		t.Fatalf("expected an error, got %s", msg.Result)
	}
	if msg.Error.Code != CodeInvalidParams || msg.Error.Message != "cwd is required" {
		t.Errorf("error = %+v", msg.Error)
	}
}

func TestUnparseableInputIsReportedNotFatal(t *testing.T) {
	w, out, _ := pipeConn(t, func(context.Context, string, json.RawMessage) (any, error) {
		return struct{}{}, nil
	})

	send(t, w, `{not json`)
	if msg := nextMessage(t, out); msg.Error == nil || msg.Error.Code != CodeParseError {
		t.Errorf("expected a parse error, got %+v", msg)
	}

	// The connection survives it, which is what stops one bad line ending the
	// session.
	send(t, w, `{"jsonrpc":"2.0","id":"a1","method":"ping"}`)
	if got := nextMessage(t, out); string(got.ID) != `"a1"` {
		t.Errorf("the connection did not recover: %+v", got)
	}
}

// A slow request must not stop the connection noticing the cancel that arrives
// while it runs — which is the whole reason requests are handled off the read
// loop.
func TestALongRequestDoesNotBlockTheReadLoop(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan string, 2)

	w, _, _ := pipeConn(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		arrived <- method
		if method == "slow" {
			<-release
		}
		return struct{}{}, nil
	})

	send(t, w, `{"jsonrpc":"2.0","id":"a1","method":"slow"}`)
	if got := <-arrived; got != "slow" {
		t.Fatalf("first was %q", got)
	}

	send(t, w, `{"jsonrpc":"2.0","method":"session/cancel"}`)
	select {
	case got := <-arrived:
		if got != "session/cancel" {
			t.Errorf("second was %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel never arrived while a request was running")
	}
	close(release)
}

// --- adapter ---

func testAgent(t *testing.T) (*Agent, *io.PipeWriter, <-chan Message) {
	t.Helper()
	t.Setenv(config.EnvAgentDir, t.TempDir())

	a := NewAgent(func(ctx context.Context, cwd string) (*coding.Session, error) {
		return coding.New(ctx, coding.Options{Cwd: cwd, NoTools: true, NoSession: true})
	}, "test")

	serverR, clientW := io.Pipe()
	clientR, serverW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	conn := a.Attach(serverR, serverW)
	go func() { _ = conn.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = clientW.Close()
		_ = serverW.Close()
	})

	return a, clientW, drain(clientR)
}

func TestInitializeAnswersWithTheVersionAndCapabilities(t *testing.T) {
	_, w, out := testAgent(t)

	send(t, w, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1}}`)
	msg := nextMessage(t, out)

	var res InitializeResponse
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		t.Fatalf("decoding: %v (%s)", err, msg.Result)
	}
	if res.ProtocolVersion != ProtocolVersion {
		t.Errorf("version = %d, want %d", res.ProtocolVersion, ProtocolVersion)
	}
	if res.AgentInfo == nil || res.AgentInfo.Name != "tau" {
		t.Errorf("agentInfo = %+v", res.AgentInfo)
	}
	// Claiming loadSession would promise a resume that is not wired.
	if res.AgentCapabilities.LoadSession {
		t.Error("loadSession is advertised but not implemented")
	}
	if !res.AgentCapabilities.PromptCapabilities.Image {
		t.Error("images are accepted but not advertised")
	}
}

func TestStartingASessionReturnsAnID(t *testing.T) {
	_, w, out := testAgent(t)

	send(t, w, `{"jsonrpc":"2.0","id":"1","method":"session/new","params":{"cwd":"`+t.TempDir()+`","mcpServers":[]}}`)
	msg := nextMessage(t, out)
	if msg.Error != nil {
		t.Fatalf("error: %+v", msg.Error)
	}

	var res NewSessionResponse
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.SessionID == "" {
		t.Error("no session id came back")
	}
}

func TestSessionNewRequiresACwd(t *testing.T) {
	_, w, out := testAgent(t)

	send(t, w, `{"jsonrpc":"2.0","id":"1","method":"session/new","params":{"mcpServers":[]}}`)
	msg := nextMessage(t, out)
	if msg.Error == nil || msg.Error.Code != CodeInvalidParams {
		t.Errorf("expected invalid params, got %+v", msg)
	}
}

func TestPromptingAnUnknownSessionFails(t *testing.T) {
	_, w, out := testAgent(t)

	send(t, w, `{"jsonrpc":"2.0","id":"1","method":"session/prompt","params":{"sessionId":"nope","prompt":[]}}`)
	if msg := nextMessage(t, out); msg.Error == nil {
		t.Errorf("expected an error for an unknown session, got %s", msg.Result)
	}
}

func TestAnUnsupportedMethodSaysSo(t *testing.T) {
	_, w, out := testAgent(t)

	send(t, w, `{"jsonrpc":"2.0","id":"1","method":"terminal/create","params":{}}`)
	msg := nextMessage(t, out)
	if msg.Error == nil || msg.Error.Code != CodeMethodNotFound {
		t.Errorf("expected method-not-found, got %+v", msg)
	}
}

// --- conversions ---

func TestPromptContentConversion(t *testing.T) {
	// Text alone stays a string, which is the shape a session file records for
	// a text-only turn.
	plain := promptContent([]ContentBlock{{Type: "text", Text: "hello"}})
	if plain.Blocks != nil || plain.Text != "hello" {
		t.Errorf("text-only = %+v", plain)
	}

	// Several blocks join rather than becoming several messages.
	joined := promptContent([]ContentBlock{
		{Type: "text", Text: "first"},
		{Type: "resource_link", URI: "/abs/path.go"},
	})
	if joined.Text != "first\n/abs/path.go" {
		t.Errorf("joined = %q", joined.Text)
	}

	// An image forces the block form, with the text first.
	withImage := promptContent([]ContentBlock{
		{Type: "text", Text: "what is this"},
		{Type: "image", Data: "aGk=", MimeType: "image/png"},
	})
	if len(withImage.Blocks) != 2 {
		t.Fatalf("blocks = %+v", withImage.Blocks)
	}
	if _, ok := withImage.Blocks[0].(ai.TextContent); !ok {
		t.Errorf("first block = %#v, want the text", withImage.Blocks[0])
	}
	if _, ok := withImage.Blocks[1].(ai.ImageContent); !ok {
		t.Errorf("second block = %#v, want the image", withImage.Blocks[1])
	}
}

func TestStopReasonMapping(t *testing.T) {
	for _, tc := range []struct {
		reason ai.StopReason
		want   string
	}{
		{ai.StopStop, StopEndTurn},
		{ai.StopToolUse, StopEndTurn},
		{ai.StopLength, StopMaxTokens},
		{ai.StopAborted, StopCancelled},
	} {
		got := stopReason([]ai.Message{ai.AssistantMessage{StopReason: tc.reason}})
		if got != tc.want {
			t.Errorf("%s -> %q, want %q", tc.reason, got, tc.want)
		}
	}
	// Nothing to go on is a completed turn rather than a failure.
	if got := stopReason(nil); got != StopEndTurn {
		t.Errorf("empty -> %q", got)
	}
}

func TestToolOutputBecomesContentBlocks(t *testing.T) {
	out := toolContent(&agent.ToolResult{Content: ai.ContentList{
		ai.TextContent{Text: "the output"},
		ai.TextContent{Text: "   "},
		ai.ImageContent{Data: "aGk=", MimeType: "image/png"},
		ai.ImageContent{Data: "not base64!!", MimeType: "image/png"},
	}})

	if len(out) != 2 {
		t.Fatalf("got %d blocks, want the text and the valid image: %+v", len(out), out)
	}
	if out[0].Content.Text != "the output" {
		t.Errorf("first = %+v", out[0])
	}
	if out[1].Content.Type != "image" {
		t.Errorf("second = %+v", out[1])
	}
	if toolContent(nil) != nil {
		t.Error("a nil result produced content")
	}
}

// The event translation is what a client actually renders, so each kind has to
// arrive under the discriminator the schema names.
func TestAgentEventsBecomeSessionUpdates(t *testing.T) {
	a, _, out := testAgent(t)
	live := &liveSession{id: "s1"}

	a.forward(live, agent.Event{
		Type:        agent.EventMessageUpdate,
		StreamEvent: &ai.Event{Type: ai.EventTextDelta, Delta: "hello"},
	})
	assertUpdate(t, out, UpdateAgentMessageChunk, "hello")

	a.forward(live, agent.Event{
		Type:        agent.EventMessageUpdate,
		StreamEvent: &ai.Event{Type: ai.EventThinkingDelta, Delta: "hmm"},
	})
	assertUpdate(t, out, UpdateAgentThoughtChunk, "hmm")

	a.forward(live, agent.Event{
		Type: agent.EventToolExecutionStart, ToolCallID: "t1", ToolName: "read",
	})
	assertUpdate(t, out, UpdateToolCall, "t1")

	a.forward(live, agent.Event{
		Type: agent.EventToolExecutionEnd, ToolCallID: "t1", IsError: true,
	})
	assertUpdate(t, out, UpdateToolCallUpdate, ToolFailed)
}

// A tool-call argument delta is not a message chunk: the call is reported once
// it starts, with its arguments whole.
func TestToolArgumentDeltasAreNotMessageChunks(t *testing.T) {
	a, _, out := testAgent(t)
	live := &liveSession{id: "s1"}

	a.forward(live, agent.Event{
		Type:        agent.EventMessageUpdate,
		StreamEvent: &ai.Event{Type: ai.EventToolCallDelta, Delta: `{"pa`},
	})
	// Nothing should have been written, so the next event's update is first.
	a.forward(live, agent.Event{
		Type:        agent.EventMessageUpdate,
		StreamEvent: &ai.Event{Type: ai.EventTextDelta, Delta: "real text"},
	})
	assertUpdate(t, out, UpdateAgentMessageChunk, "real text")
}

func assertUpdate(t *testing.T, out <-chan Message, wantKind, wantSubstring string) {
	t.Helper()
	msg := nextMessage(t, out)
	if msg.Method != MethodSessionUpdate {
		t.Fatalf("method = %q, want %q", msg.Method, MethodSessionUpdate)
	}
	body := string(msg.Params)
	if !strings.Contains(body, `"sessionUpdate":"`+wantKind+`"`) {
		t.Errorf("update kind missing %q: %s", wantKind, body)
	}
	if !strings.Contains(body, wantSubstring) {
		t.Errorf("update missing %q: %s", wantSubstring, body)
	}
	if !strings.Contains(body, `"sessionId":"s1"`) {
		t.Errorf("update is not addressed to the session: %s", body)
	}
}
