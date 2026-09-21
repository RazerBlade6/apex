package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedProject(t *testing.T, st *Store, slug string) Project {
	t.Helper()
	p := Project{
		Slug:         slug,
		Name:         slug,
		Path:         "~/Development/" + slug,
		Status:       "active",
		RegisteredAt: time.Now().Add(-24 * time.Hour).Truncate(time.Millisecond),
	}
	if err := st.UpsertProject(context.Background(), p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	return p
}

func TestProjectRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	want := seedProject(t, st, "scholarrag")
	got, err := st.GetProject(ctx, "scholarrag")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Name != want.Name || got.Path != want.Path || got.Status != want.Status {
		t.Errorf("GetProject = %+v, want %+v", got, want)
	}
	if !got.RegisteredAt.Equal(want.RegisteredAt) {
		t.Errorf("RegisteredAt = %v, want %v", got.RegisteredAt, want.RegisteredAt)
	}
	if got.LastSyncedAt != nil {
		t.Errorf("LastSyncedAt = %v, want nil", got.LastSyncedAt)
	}

	// Upsert preserves registered_at and updates the mutable fields.
	updated := want
	updated.Status = "paused"
	updated.Name = "ScholarRAG"
	updated.RegisteredAt = time.Now()
	if err := st.UpsertProject(ctx, updated); err != nil {
		t.Fatalf("second UpsertProject: %v", err)
	}
	got, err = st.GetProject(ctx, "scholarrag")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "paused" || got.Name != "ScholarRAG" {
		t.Errorf("upsert did not update mutable fields: %+v", got)
	}
	if !got.RegisteredAt.Equal(want.RegisteredAt) {
		t.Errorf("upsert changed RegisteredAt to %v", got.RegisteredAt)
	}

	syncedAt := time.Now().Truncate(time.Millisecond)
	if err := st.MarkProjectSynced(ctx, "scholarrag", syncedAt); err != nil {
		t.Fatalf("MarkProjectSynced: %v", err)
	}
	got, err = st.GetProject(ctx, "scholarrag")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSyncedAt == nil || !got.LastSyncedAt.Equal(syncedAt) {
		t.Errorf("LastSyncedAt = %v, want %v", got.LastSyncedAt, syncedAt)
	}

	seedProject(t, st, "apex")
	list, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(list) != 2 || list[0].Slug != "apex" || list[1].Slug != "scholarrag" {
		t.Errorf("ListProjects = %+v, want apex then scholarrag", list)
	}
}

func TestNotFound(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	tests := []struct {
		name string
		call func() error
	}{
		{"project", func() error { _, err := st.GetProject(ctx, "nope"); return err }},
		{"digest", func() error { _, err := st.GetDigest(ctx, "nope"); return err }},
		{"action item", func() error { _, err := st.GetActionItem(ctx, "AI-999"); return err }},
		{"idea", func() error { _, err := st.GetIdea(ctx, "IDEA-999"); return err }},
		{"session", func() error { _, err := st.GetSession(ctx, "nope"); return err }},
		{"exec run", func() error { _, err := st.GetExecRun(ctx, "nope"); return err }},
		{"status update", func() error { return st.SetActionItemStatus(ctx, "AI-999", ItemDone, time.Now()) }},
		{"mark synced", func() error { return st.MarkProjectSynced(ctx, "nope", time.Now()) }},
		{"finish run", func() error { return st.FinishExecRun(ctx, "nope", ExitSuccess, time.Now()) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestDigestRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedProject(t, st, "apex")

	d := Digest{
		ProjectSlug: "apex",
		Body:        "Personal project management hub. Building the v1 skeleton.",
		SourceHash:  "abc123",
		GeneratedAt: time.Now().Truncate(time.Millisecond),
		Model:       "claude-opus-5",
	}
	if err := st.UpsertDigest(ctx, d); err != nil {
		t.Fatalf("UpsertDigest: %v", err)
	}
	got, err := st.GetDigest(ctx, "apex")
	if err != nil {
		t.Fatalf("GetDigest: %v", err)
	}
	if got.Body != d.Body || got.SourceHash != d.SourceHash || got.Model != d.Model {
		t.Errorf("GetDigest = %+v, want %+v", got, d)
	}
	if !got.GeneratedAt.Equal(d.GeneratedAt) {
		t.Errorf("GeneratedAt = %v, want %v", got.GeneratedAt, d.GeneratedAt)
	}

	// A digest is replaced, not duplicated: the table is keyed by project.
	d.Body = "regenerated"
	d.SourceHash = "def456"
	if err := st.UpsertDigest(ctx, d); err != nil {
		t.Fatal(err)
	}
	all, err := st.ListDigests(ctx)
	if err != nil {
		t.Fatalf("ListDigests: %v", err)
	}
	if len(all) != 1 || all[0].Body != "regenerated" {
		t.Errorf("ListDigests = %+v, want one regenerated digest", all)
	}
}

func TestActionItemLifecycle(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedProject(t, st, "apex")
	seedProject(t, st, "scholarrag")

	now := time.Now().Truncate(time.Millisecond)
	items := []ActionItem{
		{ID: "AI-001", ProjectSlug: "apex", Title: "Wire up doctor", Status: ItemProposed,
			Body: "body", Rationale: "why", Effort: "small",
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
			GeneratedBy: "claude-opus-5", ContextHash: "ctx-1"},
		{ID: "AI-002", ProjectSlug: "scholarrag", Title: "Table-aware chunking", Status: ItemAccepted,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
		{ID: "AI-003", ProjectSlug: "apex", Title: "Migrations", Status: ItemProposed,
			CreatedAt: now, UpdatedAt: now},
	}
	for _, it := range items {
		if err := st.InsertActionItem(ctx, it); err != nil {
			t.Fatalf("InsertActionItem %s: %v", it.ID, err)
		}
	}

	got, err := st.GetActionItem(ctx, "AI-001")
	if err != nil {
		t.Fatalf("GetActionItem: %v", err)
	}
	want := items[0]
	if got.Title != want.Title || got.Body != want.Body || got.Rationale != want.Rationale ||
		got.Effort != want.Effort || got.GeneratedBy != want.GeneratedBy || got.ContextHash != want.ContextHash {
		t.Errorf("GetActionItem = %+v, want %+v", got, want)
	}

	// Optional fields left empty come back empty, not as a literal "".
	sparse, err := st.GetActionItem(ctx, "AI-002")
	if err != nil {
		t.Fatal(err)
	}
	if sparse.Body != "" || sparse.ContextHash != "" {
		t.Errorf("sparse item = %+v, want empty optional fields", sparse)
	}

	filters := []struct {
		name  string
		f     ActionItemFilter
		wants []string
	}{
		{"all", ActionItemFilter{}, []string{"AI-003", "AI-002", "AI-001"}},
		{"by status", ActionItemFilter{Status: ItemProposed}, []string{"AI-003", "AI-001"}},
		{"by project", ActionItemFilter{ProjectSlug: "apex"}, []string{"AI-003", "AI-001"}},
		{"by both", ActionItemFilter{ProjectSlug: "apex", Status: ItemProposed}, []string{"AI-003", "AI-001"}},
		{"no match", ActionItemFilter{Status: ItemDone}, nil},
	}
	for _, tt := range filters {
		t.Run(tt.name, func(t *testing.T) {
			got, err := st.ListActionItems(ctx, tt.f)
			if err != nil {
				t.Fatalf("ListActionItems: %v", err)
			}
			if len(got) != len(tt.wants) {
				t.Fatalf("got %d items, want %d (%v)", len(got), len(tt.wants), tt.wants)
			}
			for i, id := range tt.wants {
				if got[i].ID != id {
					t.Errorf("item %d = %s, want %s", i, got[i].ID, id)
				}
			}
		})
	}

	later := now.Add(time.Minute)
	if err := st.SetActionItemStatus(ctx, "AI-001", ItemInReview, later); err != nil {
		t.Fatalf("SetActionItemStatus: %v", err)
	}
	got, err = st.GetActionItem(ctx, "AI-001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ItemInReview || !got.UpdatedAt.Equal(later) {
		t.Errorf("after status change = %s at %v, want in_review at %v", got.Status, got.UpdatedAt, later)
	}

	next, err := st.NextActionItemID(ctx)
	if err != nil {
		t.Fatalf("NextActionItemID: %v", err)
	}
	if next != "AI-004" {
		t.Errorf("NextActionItemID = %s, want AI-004", next)
	}
}

func TestIdeaRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedProject(t, st, "apex")

	now := time.Now().Truncate(time.Millisecond)
	if err := st.InsertIdea(ctx, Idea{
		ID: "IDEA-001", Title: "Digest diffing", Pitch: "show what changed",
		Rationale: "fits your habits", Status: IdeaProposed, CreatedAt: now,
		GeneratedBy: "claude-opus-5",
	}); err != nil {
		t.Fatalf("InsertIdea: %v", err)
	}

	got, err := st.GetIdea(ctx, "IDEA-001")
	if err != nil {
		t.Fatalf("GetIdea: %v", err)
	}
	if got.Title != "Digest diffing" || got.Pitch != "show what changed" || got.StartedProjectSlug != "" {
		t.Errorf("GetIdea = %+v", got)
	}

	if err := st.SetIdeaStatus(ctx, "IDEA-001", IdeaStarted, "apex"); err != nil {
		t.Fatalf("SetIdeaStatus: %v", err)
	}
	got, err = st.GetIdea(ctx, "IDEA-001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != IdeaStarted || got.StartedProjectSlug != "apex" {
		t.Errorf("after SetIdeaStatus = %+v, want started/apex", got)
	}

	proposed, err := st.ListIdeas(ctx, IdeaProposed)
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	if len(proposed) != 0 {
		t.Errorf("ListIdeas(proposed) = %+v, want none", proposed)
	}
	all, err := st.ListIdeas(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("ListIdeas(all) returned %d ideas, want 1", len(all))
	}

	next, err := st.NextIdeaID(ctx)
	if err != nil {
		t.Fatalf("NextIdeaID: %v", err)
	}
	if next != "IDEA-002" {
		t.Errorf("NextIdeaID = %s, want IDEA-002", next)
	}
}

func TestNextIDOnEmptyTable(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	if got, err := st.NextActionItemID(ctx); err != nil || got != "AI-001" {
		t.Errorf("NextActionItemID = %q, %v; want AI-001", got, err)
	}
	if got, err := st.NextIdeaID(ctx); err != nil || got != "IDEA-001" {
		t.Errorf("NextIdeaID = %q, %v; want IDEA-001", got, err)
	}
}

func TestSessionsAndMessages(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	now := time.Now().Truncate(time.Millisecond)
	if err := st.InsertSession(ctx, Session{ID: "s1", Title: "Planning", StartedAt: now}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	sess, err := st.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Title != "Planning" || !sess.StartedAt.Equal(now) {
		t.Errorf("GetSession = %+v", sess)
	}

	in, out := 1200, 340
	first, err := st.AppendMessage(ctx, Message{
		SessionID: "s1", Role: RoleUser, Content: "what should I work on?", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	second, err := st.AppendMessage(ctx, Message{
		SessionID: "s1", Role: RoleAssistant, Content: "finish M1", CreatedAt: now.Add(time.Second),
		Model: "claude-opus-5", InputTokens: &in, OutputTokens: &out,
	})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if second <= first {
		t.Errorf("message IDs are not increasing: %d then %d", first, second)
	}

	msgs, err := st.ListMessages(ctx, "s1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("ListMessages returned %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != RoleUser || msgs[0].InputTokens != nil {
		t.Errorf("first message = %+v, want a user turn with no usage", msgs[0])
	}
	if msgs[1].InputTokens == nil || *msgs[1].InputTokens != in ||
		msgs[1].OutputTokens == nil || *msgs[1].OutputTokens != out {
		t.Errorf("second message usage = %+v, want %d/%d", msgs[1], in, out)
	}

	sessions, err := st.ListSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("ListSessions returned %d, want 1", len(sessions))
	}
}

func TestExecRunLifecycle(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	seedProject(t, st, "apex")

	now := time.Now().Truncate(time.Millisecond)
	if err := st.InsertActionItem(ctx, ActionItem{
		ID: "AI-001", ProjectSlug: "apex", Title: "t", Status: ItemInProgress,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertExecRun(ctx, ExecRun{
		ID: "7f3a91", ActionItemID: "AI-001", Executor: "claudecode",
		StartedAt: now, LogPath: "~/.apex/logs/exec/7f3a91.log",
	}); err != nil {
		t.Fatalf("InsertExecRun: %v", err)
	}

	run, err := st.GetExecRun(ctx, "7f3a91")
	if err != nil {
		t.Fatalf("GetExecRun: %v", err)
	}
	if run.FinishedAt != nil || run.ExitStatus != "" {
		t.Errorf("new run = %+v, want unfinished", run)
	}

	finished := now.Add(4 * time.Minute)
	if err := st.FinishExecRun(ctx, "7f3a91", ExitSuccess, finished); err != nil {
		t.Fatalf("FinishExecRun: %v", err)
	}
	run, err = st.GetExecRun(ctx, "7f3a91")
	if err != nil {
		t.Fatal(err)
	}
	if run.ExitStatus != ExitSuccess || run.FinishedAt == nil || !run.FinishedAt.Equal(finished) {
		t.Errorf("finished run = %+v, want success at %v", run, finished)
	}

	runs, err := st.ListExecRuns(ctx, "AI-001")
	if err != nil {
		t.Fatalf("ListExecRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "7f3a91" {
		t.Errorf("ListExecRuns = %+v, want the one run", runs)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	// An action item against an unregistered project is a bug worth catching
	// at the database rather than discovering later in a join.
	err := st.InsertActionItem(ctx, ActionItem{
		ID: "AI-001", ProjectSlug: "ghost", Title: "t", Status: ItemProposed,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("inserted an action item for a nonexistent project")
	}
}

func TestTimeRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
	}{
		{"utc", time.Date(2026, 9, 21, 10, 4, 5, 0, time.UTC)},
		{"with nanos", time.Date(2026, 9, 21, 10, 4, 5, 123456789, time.UTC)},
		{"non-utc zone", time.Date(2026, 9, 21, 10, 4, 5, 0, time.FixedZone("PDT", -7*3600))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTime(formatTime(tt.in))
			if err != nil {
				t.Fatalf("parseTime: %v", err)
			}
			if !got.Equal(tt.in) {
				t.Errorf("round trip = %v, want %v", got, tt.in)
			}
		})
	}

	// Second-precision RFC3339, as another tool might write it.
	if got, err := parseTime("2026-09-21T10:04:05Z"); err != nil || !got.Equal(time.Date(2026, 9, 21, 10, 4, 5, 0, time.UTC)) {
		t.Errorf("parseTime(RFC3339) = %v, %v", got, err)
	}
	if _, err := parseTime("yesterday"); err == nil {
		t.Error("parseTime accepted a non-timestamp")
	}
}
