package coding

import (
	"context"
	"strings"
	"testing"
)

func TestSessionEnvDescribesTheRunningSession(t *testing.T) {
	cs := newTestSession(t, Options{})

	vars := map[string]string{}
	for _, kv := range cs.sessionEnv() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("%q is not a KEY=VALUE pair", kv)
		}
		vars[k] = v
	}

	if vars["TAU_MODEL"] != cs.Model.ID {
		t.Errorf("TAU_MODEL = %q, want %q", vars["TAU_MODEL"], cs.Model.ID)
	}
	if vars["TAU_PROVIDER"] != string(cs.Model.Provider) {
		t.Errorf("TAU_PROVIDER = %q, want %q", vars["TAU_PROVIDER"], cs.Model.Provider)
	}
	if vars["TAU_SESSION_ID"] == "" {
		t.Error("TAU_SESSION_ID is empty for a persisted session")
	}
	if vars["TAU_SESSION_FILE"] != cs.Path {
		t.Errorf("TAU_SESSION_FILE = %q, want %q", vars["TAU_SESSION_FILE"], cs.Path)
	}

	// Empty values are dropped here rather than written blank: the bash tool
	// blanks the whole set first, so a missing key already reads as unset.
	for k, v := range vars {
		if v == "" {
			t.Errorf("%s is present but empty", k)
		}
	}
}

// The variables are read per command, so switching models mid-session must
// change what the next command sees.
func TestSessionEnvFollowsTheModel(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	before := cs.sessionEnv()
	other := ""
	for _, m := range cs.Models.Models() {
		if m.ID != cs.Model.ID {
			other = string(m.Provider) + "/" + m.ID
			break
		}
	}
	if other == "" {
		t.Skip("the test registry has only one model")
	}
	if _, err := cs.SetModel(ctx, other); err != nil {
		t.Fatal(err)
	}

	after := cs.sessionEnv()
	if strings.Join(before, " ") == strings.Join(after, " ") {
		t.Error("the environment did not follow the model switch")
	}
	if !strings.Contains(strings.Join(after, " "), "TAU_MODEL="+cs.Model.ID) {
		t.Errorf("environment = %q, want the new model", after)
	}
}
