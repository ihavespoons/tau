package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The P7 gate, driven through the whole binary: the session operations that
// change what the model can see have to work from the keyboard, not just from
// the library.

// p7Env is tauEnv with a state directory the test can also write settings into.
func p7Env(t *testing.T, baseURL, settings string) (agentDir string, env []string) {
	t.Helper()
	agentDir = t.TempDir()
	if settings != "" {
		if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return agentDir, []string{
		"TAU_AGENT_DIR=" + agentDir,
		"ANTHROPIC_BASE_URL=" + baseURL,
		"ANTHROPIC_API_KEY=sk-ant-test",
	}
}

// sessionFile finds the single session tau wrote under agentDir.
func sessionFile(t *testing.T, agentDir string) string {
	t.Helper()
	var found string
	err := filepath.Walk(filepath.Join(agentDir, "sessions"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		found = path
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("no session file under %s (%v)", agentDir, err)
	}
	return found
}

// /compact has to summarize the conversation into a checkpoint entry, and the
// summary has to come from the provider — this is the command a user reaches
// for when a long session stops fitting.
func TestCompactCommandWritesACheckpoint(t *testing.T) {
	// Two very different answers so it is obvious which request produced which.
	url, calls := fakeAnthropic(t,
		writeStream(textStream("the first answer")),
		writeStream(textStream("## Goal\nSUMMARY FROM THE PROVIDER")),
	)
	// A tiny retained tail, so a short conversation is enough to compact.
	agentDir, env := p7Env(t, url, `{"compaction":{"keepRecentTokens":1,"reserveTokens":500}}`)

	s := startTau(t, t.TempDir(), env...)
	s.waitFor("/help for commands", 10*time.Second)

	s.send("tell me something\r")
	s.waitFor("the first answer", 10*time.Second)

	s.send("/compact\r")
	s.waitFor("Compacted", 15*time.Second)

	if got := calls.Load(); got != 2 {
		t.Errorf("provider requests = %d, want 2 (the turn and the summary)", got)
	}

	data, err := os.ReadFile(sessionFile(t, agentDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type":"compaction"`) {
		t.Errorf("no compaction entry in the session file:\n%s", data)
	}
	if !strings.Contains(string(data), "SUMMARY FROM THE PROVIDER") {
		t.Errorf("the provider's summary is not in the checkpoint:\n%s", data)
	}
}

// /tree has to show the conversation's shape. Without an argument and with a
// UI it opens a picker, so the assertion is on the picker appearing with the
// prompts in it.
func TestTreeCommandOpensTheBranchPicker(t *testing.T) {
	url, _ := fakeAnthropic(t, writeStream(textStream("an answer")))
	_, env := p7Env(t, url, "")

	s := startTau(t, t.TempDir(), env...)
	s.waitFor("/help for commands", 10*time.Second)

	s.send("the first question\r")
	s.waitFor("an answer", 10*time.Second)

	s.send("/tree\r")
	s.waitFor("Go back to", 10*time.Second)
	s.waitFor("the first question", 5*time.Second)

	s.send("\x1b") // Esc closes the picker without moving.
}

// /fork copies the session and switches to the copy. The original file has to
// still be there afterwards — that is the whole point of forking rather than
// rewinding.
func TestForkCommandLeavesTheOriginalBehind(t *testing.T) {
	url, _ := fakeAnthropic(t, writeStream(textStream("an answer")))
	agentDir, env := p7Env(t, url, "")

	s := startTau(t, t.TempDir(), env...)
	s.waitFor("/help for commands", 10*time.Second)

	s.send("the first question\r")
	s.waitFor("an answer", 10*time.Second)

	original := sessionFile(t, agentDir)

	s.send("/fork\r")
	s.waitFor("Fork from", 10*time.Second)
	s.send("\r") // take the highlighted prompt
	s.waitFor("Forked to", 15*time.Second)

	if _, err := os.Stat(original); err != nil {
		t.Errorf("the original session is gone: %v", err)
	}

	count := 0
	err := filepath.Walk(filepath.Join(agentDir, "sessions"), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("found %d session files, want the original and the fork", count)
	}
}

// The recovery that matters: the provider rejects a turn for length, tau
// compacts and sends it again, and the user sees an answer rather than an
// error. Nothing about this is visible in the UI except that it worked.
func TestATurnRejectedForLengthRecoversInTheTUI(t *testing.T) {
	rejected := false
	reject := func(w http.ResponseWriter, _ *http.Request) {
		rejected = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 213462 tokens > 200000 maximum"}}`))
	}

	url, _ := fakeAnthropic(t,
		writeStream(textStream("the first answer")),
		reject,
		writeStream(textStream("SUMMARY OF THE HISTORY")),
		writeStream(textStream("the recovered answer")),
	)
	agentDir, env := p7Env(t, url, `{"compaction":{"keepRecentTokens":1,"reserveTokens":500}}`)

	s := startTau(t, t.TempDir(), env...)
	s.waitFor("/help for commands", 10*time.Second)

	s.send("tell me something\r")
	s.waitFor("the first answer", 10*time.Second)

	s.send("now the long one\r")
	s.waitFor("the recovered answer", 20*time.Second)

	if !rejected {
		t.Error("the rejection never happened, so nothing was recovered from")
	}
	data, err := os.ReadFile(sessionFile(t, agentDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type":"compaction"`) {
		t.Error("no checkpoint was written before the retry")
	}
}
