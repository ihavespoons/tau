// Command echoserver is a minimal MCP server used by tau's end-to-end test.
//
// It is a separate program on purpose: the test then exercises the real path a
// user's server takes — process launch, stdio framing, initialization, tool
// discovery, and a round-trip call — instead of an in-process shortcut.
package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type shoutInput struct {
	Text string `json:"text" jsonschema:"the text to shout"`
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "echoserver", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "shout",
		Description: "Return the given text in upper case with an exclamation mark.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in shoutInput) (*mcp.CallToolResult, any, error) {
		out := ""
		for _, r := range in.Text {
			if r >= 'a' && r <= 'z' {
				r -= 32
			}
			out += string(r)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: out + "!"}},
		}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}
