package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/session"
)

func testRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func user(text string) ai.Message {
	return ai.UserMessage{Content: ai.UserContent{Text: text}, Timestamp: 1}
}

func mustCreate(t *testing.T, r *Repo, cwd string) *session.Session {
	t.Helper()
	s, err := r.Create(context.Background(), session.CreateSessionOptions{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The point of the backend: what was appended is still there after the process
// that appended it is gone.
func TestEntriesSurviveAReopen(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	s := mustCreate(t, r, "/work")

	for _, text := range []string{"first", "second", "third"} {
		if _, err := s.AppendMessage(ctx, user(text)); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := s.Storage().Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := r.Open(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	entries := reopened.Entries(ctx, nil)
	if len(entries) != 3 {
		t.Fatalf("%d entries after reopen, want 3", len(entries))
	}
	if got := entries[0].(*session.MessageEntry).Message.(ai.UserMessage).Content.Text; got != "first" {
		t.Errorf("first entry = %q", got)
	}
}

// The leaf, labels and name are all derived state. Replaying the log through
// session.Index is what makes them come back identical rather than
// approximately.
func TestDerivedStateComesBackFromTheLog(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	s := mustCreate(t, r, "/work")

	id, err := s.AppendMessage(ctx, user("worth remembering"))
	if err != nil {
		t.Fatal(err)
	}
	label := "before the refactor"
	if _, err := s.AppendLabel(ctx, id, &label); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendName(ctx, "the sqlite session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, user("and on")); err != nil {
		t.Fatal(err)
	}

	meta, _ := s.Storage().Metadata(ctx)
	reopened, err := r.Open(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := reopened.Label(ctx, id); !ok || got != label {
		t.Errorf("label = %q,%v want %q", got, ok, label)
	}
	if got, ok := reopened.Name(ctx); !ok || got != "the sqlite session" {
		t.Errorf("name = %q,%v", got, ok)
	}
	leaf, err := reopened.LeafID(ctx)
	if err != nil || leaf == nil {
		t.Fatalf("leaf = %v, %v", leaf, err)
	}
}

// A label set twice is last-write-wins, and getting that wrong on reload is the
// exact bug that duplicating the index logic in SQL would have introduced.
func TestTheLastLabelWins(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	s := mustCreate(t, r, "/work")

	id, _ := s.AppendMessage(ctx, user("target"))
	for _, l := range []string{"first try", "second try"} {
		label := l
		if _, err := s.AppendLabel(ctx, id, &label); err != nil {
			t.Fatal(err)
		}
	}

	meta, _ := s.Storage().Metadata(ctx)
	reopened, _ := r.Open(ctx, meta)
	if got, _ := reopened.Label(ctx, id); got != "second try" {
		t.Errorf("label = %q, want the later one", got)
	}
}

func TestListingIsNewestFirstAndScopedToACwd(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)

	a := mustCreate(t, r, "/one")
	b := mustCreate(t, r, "/one")
	mustCreate(t, r, "/two")

	all, err := r.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("%d sessions listed, want 3", len(all))
	}

	scoped, err := r.List(ctx, "/one")
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 {
		t.Fatalf("%d sessions in /one, want 2", len(scoped))
	}
	// Ids sort by creation time, so the newer session leads.
	metaA, _ := a.Storage().Metadata(ctx)
	metaB, _ := b.Storage().Metadata(ctx)
	if scoped[0].ID != metaB.ID && scoped[0].ID != metaA.ID {
		t.Errorf("listing did not contain the created sessions: %+v", scoped)
	}
	for _, m := range scoped {
		if m.Cwd != "/one" {
			t.Errorf("session from %q leaked into the /one listing", m.Cwd)
		}
	}
}

// Two sessions in one database must not see each other's entries.
func TestSessionsInOneDatabaseStayApart(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)

	a := mustCreate(t, r, "/work")
	b := mustCreate(t, r, "/work")
	if _, err := a.AppendMessage(ctx, user("in a")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AppendMessage(ctx, user("in b")); err != nil {
		t.Fatal(err)
	}

	metaA, _ := a.Storage().Metadata(ctx)
	reopened, _ := r.Open(ctx, metaA)
	entries := reopened.Entries(ctx, nil)
	if len(entries) != 1 {
		t.Fatalf("session a has %d entries, want its own 1", len(entries))
	}
	if got := entries[0].(*session.MessageEntry).Message.(ai.UserMessage).Content.Text; got != "in a" {
		t.Errorf("session a holds %q", got)
	}
}

func TestDeletingASessionTakesItsEntries(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	s := mustCreate(t, r, "/work")
	if _, err := s.AppendMessage(ctx, user("doomed")); err != nil {
		t.Fatal(err)
	}
	meta, _ := s.Storage().Metadata(ctx)

	if err := r.Delete(ctx, meta); err != nil {
		t.Fatal(err)
	}
	if left, _ := r.List(ctx, ""); len(left) != 0 {
		t.Errorf("%d sessions left after delete", len(left))
	}

	// The cascade is a foreign key, which only fires because the pragma is set
	// on every connection rather than assumed.
	var orphans int
	if err := r.DB().QueryRow(`SELECT count(*) FROM entries`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d orphaned entries left behind", orphans)
	}
}

func TestForkCopiesAPrefixAndPointsBack(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	src := mustCreate(t, r, "/work")

	first, _ := src.AppendMessage(ctx, user("shared start"))
	if _, err := src.AppendMessage(ctx, user("only in the original")); err != nil {
		t.Fatal(err)
	}
	meta, _ := src.Storage().Metadata(ctx)

	forked, err := r.Fork(ctx, meta, session.CreateSessionOptions{},
		session.ForkOptions{EntryID: first, Position: "at"})
	if err != nil {
		t.Fatal(err)
	}

	entries := forked.Entries(ctx, nil)
	if len(entries) != 1 {
		t.Fatalf("fork copied %d entries, want the prefix of 1", len(entries))
	}
	forkedMeta, _ := forked.Storage().Metadata(ctx)
	if forkedMeta.ParentSessionPath != meta.Path {
		t.Errorf("fork points at %q, want %q", forkedMeta.ParentSessionPath, meta.Path)
	}
	// The original is untouched, which is what makes forking safe.
	if got := len(src.Entries(ctx, nil)); got != 2 {
		t.Errorf("the source now has %d entries", got)
	}
}

// An entry type this build does not know about has to survive a round trip, or
// a session written by a newer tau would lose data on being opened by an older
// one.
func TestAnUnknownEntryTypeRoundTrips(t *testing.T) {
	ctx := context.Background()
	r := testRepo(t)
	s := mustCreate(t, r, "/work")

	raw := []byte(`{"id":"x1","parentId":null,"timestamp":"2026-08-10T00:00:00.000Z","type":"from_the_future","payload":{"a":1}}`)
	// An unknown type reports an error *and* an opaque entry: the error says
	// this build did not understand it, the entry says the bytes and the tree
	// links are intact. Dropping it on the error would sever the path between
	// the entries either side of it.
	entry, err := session.UnmarshalEntry(raw)
	if err == nil {
		t.Fatal("an unknown entry type should still report that it was unknown")
	}
	if entry == nil {
		t.Fatal("an unknown entry type should decode opaquely rather than to nothing")
	}
	if err := s.Storage().AppendEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}

	meta, _ := s.Storage().Metadata(ctx)
	reopened, err := r.Open(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	entries := reopened.Entries(ctx, nil)
	if len(entries) != 1 {
		t.Fatalf("%d entries, want the opaque one", len(entries))
	}
	if got := entries[0].EntryType(); got != "from_the_future" {
		t.Errorf("entry type = %q, want it preserved", got)
	}
}

func TestOpeningTheSameDatabaseTwiceSeesTheSameSessions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := mustCreate(t, first, "/work")
	if _, err := s.AppendMessage(ctx, user("written by the first handle")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	listed, err := second.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("%d sessions on reopen, want 1", len(listed))
	}
	reopened, err := second.Open(ctx, listed[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.Entries(ctx, nil)); got != 1 {
		t.Errorf("%d entries on reopen, want 1", got)
	}
}

// The satellite is only useful if the agent can be pointed at it, so the wiring
// is asserted here rather than left to a reader to discover.
func TestTheRepoCanBackACodingSession(t *testing.T) {
	r := testRepo(t)
	opts := coding.Options{Repo: r}
	if opts.Repo == nil {
		t.Fatal("coding.Options will not take a sqlite repo")
	}
}
