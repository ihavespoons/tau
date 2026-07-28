package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Message string `json:"message" jsonschema:"the message to echo"`
}

// connectTestServer stands up a real MCP server over an in-memory transport
// and returns a *server wired to it, so the proxying path is exercised end to
// end rather than against a hand-rolled fake.
func connectTestServer(t *testing.T, configure func(*sdk.Server)) *server {
	t.Helper()
	ctx := context.Background()

	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	configure(srv)

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	cl := sdk.NewClient(&sdk.Implementation{Name: "tau", Version: version}, nil)
	clientSession, err := cl.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	return &server{name: "test", sess: clientSession}
}

func firstRemoteTool(t *testing.T, s *server) *remoteTool {
	t.Helper()
	res, err := s.session().ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("server exposed no tools")
	}
	tool, err := newRemoteTool(s, res.Tools[0])
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestRemoteToolProxiesCall(t *testing.T) {
	s := connectTestServer(t, func(srv *sdk.Server) {
		sdk.AddTool(srv, &sdk.Tool{Name: "echo", Description: "Echo a message"},
			func(_ context.Context, _ *sdk.CallToolRequest, in echoInput) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{
					Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + in.Message}},
				}, nil, nil
			})
	})

	tool := firstRemoteTool(t, s)

	if got := tool.Def().Name; got != "mcp__test__echo" {
		t.Errorf("tool registered as %q", got)
	}
	if tool.Def().Parameters == nil {
		t.Fatal("input schema was not converted")
	}
	if _, ok := tool.Def().Parameters.Properties["message"]; !ok {
		t.Errorf("converted schema lost the message property: %+v", tool.Def().Parameters)
	}

	res, err := tool.Execute(context.Background(), "call-1",
		json.RawMessage(`{"message":"hi"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	text, _ := res.Content[0].(ai.TextContent)
	if text.Text != "echo: hi" {
		t.Errorf("unexpected result %q", text.Text)
	}
}

// A tool that runs and fails must surface as an error result the model can
// read, not as a transport failure — that is the whole reason MCP separates
// IsError from a protocol error.
func TestRemoteToolErrorBecomesReadableResult(t *testing.T) {
	s := connectTestServer(t, func(srv *sdk.Server) {
		sdk.AddTool(srv, &sdk.Tool{Name: "boom", Description: "Always fails"},
			func(_ context.Context, _ *sdk.CallToolRequest, _ echoInput) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{
					IsError: true,
					Content: []sdk.Content{&sdk.TextContent{Text: "the file was not found"}},
				}, nil, nil
			})
	})

	tool := firstRemoteTool(t, s)
	_, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"message":"x"}`), nil)
	if err == nil {
		t.Fatal("a failing tool must report an error so the loop marks the result")
	}
	if !strings.Contains(err.Error(), "the file was not found") {
		t.Errorf("the server's message was lost: %v", err)
	}
}

// A disconnected server must fail with a clear message rather than panicking
// on a nil session.
func TestRemoteToolWithoutSession(t *testing.T) {
	tool := &remoteTool{server: &server{name: "gone"}, remote: "anything"}
	_, err := tool.Execute(context.Background(), "id", json.RawMessage(`{}`), nil)
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("expected a not-connected error, got %v", err)
	}
}

func TestConvertSchemaHandlesMissingSchema(t *testing.T) {
	s, err := convertSchema(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != "object" {
		t.Errorf("a missing schema should become an empty object schema, got %q", s.Type)
	}
}

func TestConvertContentKeepsUnknownBlocks(t *testing.T) {
	out := convertContent([]sdk.Content{
		&sdk.TextContent{Text: "hello"},
		&sdk.AudioContent{Data: []byte("x"), MIMEType: "audio/wav"},
	})
	if len(out) != 2 {
		t.Fatalf("an unrecognized block must be described, not dropped: %+v", out)
	}
	if text, ok := out[0].(ai.TextContent); !ok || text.Text != "hello" {
		t.Errorf("text block mangled: %+v", out[0])
	}
}
