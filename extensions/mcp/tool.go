package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolPrefix namespaces a server's tools. Two servers may legitimately both
// export "search"; the prefix keeps them distinct and tells the model where a
// tool came from. Underscores are used because provider tool-name validation
// rejects most other separators.
const toolPrefix = "mcp__"

// ToolName is the name a server's tool is registered under.
func ToolName(server, tool string) string { return toolPrefix + server + "__" + tool }

// SplitToolName reverses ToolName, reporting false for a name that did not
// come from an MCP server.
func SplitToolName(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, toolPrefix)
	if !found {
		return "", "", false
	}
	server, tool, found = strings.Cut(rest, "__")
	if !found || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// remoteTool adapts one MCP tool to tau's Tool interface.
type remoteTool struct {
	server *server
	def    agent.ToolDef
	remote string
}

var _ agent.Tool = (*remoteTool)(nil)

func (t *remoteTool) Def() agent.ToolDef { return t.def }

// Execute proxies the call to the server.
//
// Returning an error rather than an error result is reserved for transport
// failures. A tool that ran and failed reports that through IsError so the
// model can read the message and correct itself — the same split MCP itself
// draws.
func (t *remoteTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agent.UpdateFunc) (agent.ToolResult, error) {
	var decoded any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &decoded); err != nil {
			return agent.ToolResult{}, fmt.Errorf("decoding arguments: %w", err)
		}
	}

	sess := t.server.session()
	if sess == nil {
		return agent.ToolResult{}, fmt.Errorf("mcp server %q is not connected", t.server.name)
	}

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: t.remote, Arguments: decoded})
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("mcp server %q: %w", t.server.name, err)
	}

	out := agent.ToolResult{Content: convertContent(res.Content)}
	if res.StructuredContent != nil {
		out.Details = res.StructuredContent
		// A server may return only structured output. The model still needs
		// something to read, so serialize it as text when Content was empty.
		if len(out.Content) == 0 {
			if b, mErr := json.Marshal(res.StructuredContent); mErr == nil {
				out.Content = ai.ContentList{ai.TextContent{Text: string(b)}}
			}
		}
	}
	if res.IsError {
		return out, fmt.Errorf("%s", flattenText(out.Content))
	}
	return out, nil
}

// convertContent maps MCP content blocks to tau's.
func convertContent(blocks []mcp.Content) ai.ContentList {
	var out ai.ContentList
	for _, b := range blocks {
		switch c := b.(type) {
		case *mcp.TextContent:
			out = append(out, ai.TextContent{Text: c.Text})
		case *mcp.ImageContent:
			out = append(out, ai.ImageContent{
				Data:     base64.StdEncoding.EncodeToString(c.Data),
				MimeType: c.MIMEType,
			})
		default:
			// Audio, embedded resources, and anything added to the spec later
			// are described rather than dropped: silently losing a block the
			// server considered part of its answer is worse than a placeholder.
			if b, err := json.Marshal(c); err == nil {
				out = append(out, ai.TextContent{Text: string(b)})
			}
		}
	}
	return out
}

func flattenText(list ai.ContentList) string {
	var parts []string
	for _, c := range list {
		if t, ok := c.(ai.TextContent); ok && t.Text != "" {
			parts = append(parts, t.Text)
		}
	}
	if len(parts) == 0 {
		return "the tool reported an error with no message"
	}
	return strings.Join(parts, "\n")
}

// convertSchema turns a server's declared input schema into the schema type
// tau's tool definitions carry. Both sides use google/jsonschema-go, so this
// is a round-trip through JSON rather than a translation.
func convertSchema(raw any) (*jsonschema.Schema, error) {
	if raw == nil {
		return &jsonschema.Schema{Type: "object"}, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Type == "" && s.Properties == nil {
		s.Type = "object"
	}
	return &s, nil
}

// newRemoteTool builds a tau tool from an MCP tool declaration.
func newRemoteTool(srv *server, t *mcp.Tool) (*remoteTool, error) {
	schema, err := convertSchema(t.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("tool %q has an unusable input schema: %w", t.Name, err)
	}

	label := t.Name
	if t.Annotations != nil && t.Annotations.Title != "" {
		label = t.Annotations.Title
	}

	description := t.Description
	if description == "" {
		description = "Tool " + t.Name + " provided by the " + srv.name + " MCP server."
	}

	return &remoteTool{
		server: srv,
		remote: t.Name,
		def: agent.ToolDef{
			Name:        ToolName(srv.name, t.Name),
			Description: description,
			Label:       srv.name + ": " + label,
			Parameters:  schema,
			// The system prompt's tool section stays a curated list of tau's
			// own tools; MCP tools are advertised through the tool schema the
			// provider already receives, which is where the model reads them.
		},
	}, nil
}
