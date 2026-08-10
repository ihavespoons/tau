package tui

import (
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

// benchMessages builds a transcript with the shape a real one has: prose, a
// list, and a fenced code block per assistant turn.
func benchMessages(n int) []ai.AssistantMessage {
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 6)
	body := prose + "\n\n- first item\n- second item\n- third item\n\n" +
		"```go\nfunc handler(w http.ResponseWriter, r *http.Request) {\n" +
		"\tif err := do(r.Context()); err != nil {\n\t\thttp.Error(w, err.Error(), 500)\n\t\treturn\n\t}\n}\n```\n\n" +
		"See [the docs](https://example.com/docs) for **more**.\n"

	out := make([]ai.AssistantMessage, n)
	for i := range out {
		out[i] = ai.AssistantMessage{
			Content: []ai.Content{
				ai.ThinkingContent{Thinking: prose},
				ai.TextContent{Text: body},
			},
		}
	}
	return out
}

// Resuming a long session renders every message once. Scrolling afterwards is
// the terminal's problem — tau prints into its scrollback and never redraws —
// so this replay is the only place transcript length costs anything.
func BenchmarkReplayTranscript(b *testing.B) {
	r := newRenderer(DefaultTheme(), 100, false)
	msgs := benchMessages(1000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lines := 0
		for _, m := range msgs {
			lines += len(r.assistant(m))
		}
		if lines == 0 {
			b.Fatal("rendered nothing")
		}
	}
}

// One assistant message is what the user waits on at the end of every turn.
func BenchmarkRenderOneMessage(b *testing.B) {
	r := newRenderer(DefaultTheme(), 100, false)
	m := benchMessages(1)[0]

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(r.assistant(m)) == 0 {
			b.Fatal("rendered nothing")
		}
	}
}
