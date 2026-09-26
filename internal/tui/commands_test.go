package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestSlashReviewNeedsApproval: `/review` is `apex review`'s call, but in the
// chat nothing is written until the user ticks it.
func TestSlashReviewNeedsApproval(t *testing.T) {
	fake := &chatFake{JSON: `{"items":[
		{"project":"atlas","title":"Write a README","body":"b","rationale":"r","effort":"small"},
		{"project":"atlas","title":"Pick a status","body":"b","rationale":"r","effort":"small"}
	]}`}
	m := chatModel(t, fake)

	typeAndSend(t, m, "/review")
	if streams, structured := fake.counts(); streams != 0 || structured != 1 {
		t.Errorf("streams = %d, structured = %d; /review is one structured call", streams, structured)
	}
	if !strings.Contains(fake.requests[0].Messages[len(fake.requests[0].Messages)-1].Content, "Review the portfolio") {
		t.Error("/review did not send the review instruction")
	}
	if m.chat.proposal == nil || len(m.chat.proposal.items) != 2 {
		t.Fatalf("/review did not open a checklist: %+v", m.chat.proposal)
	}
	if n := len(storedItems(t, m)); n != 0 {
		t.Fatalf("/review recorded %d item(s) before approval", n)
	}
	pump(t, m, key(m, "y"))
	if n := len(storedItems(t, m)); n != 2 {
		t.Errorf("recorded %d item(s), want 2", n)
	}
}

// TestSlashSyncShowsTheReportAndReloads: `/sync` prints what `apex sync`
// prints, and the chat then reasons over what the sync produced.
func TestSlashSyncShowsTheReportAndReloads(t *testing.T) {
	m := chatModel(t, &chatFake{})

	typeAndSend(t, m, "/sync")
	if !strings.Contains(m.status, "not available") {
		t.Errorf("/sync without a syncer: status = %q", m.status)
	}

	var calls int
	m.opts.Sync = func(ctx context.Context, w io.Writer) error {
		calls++
		io.WriteString(w, "registry: /x/PROJECTS.md\n\nSTATE\tPROJECT\nok\tAtlas\n") //nolint:errcheck
		return nil
	}
	typeAndSend(t, m, "/sync")
	if calls != 1 || m.chat.proposing != nil {
		t.Fatalf("calls = %d, still running = %v", calls, m.chat.proposing != nil)
	}
	out := m.chat.outVP.View()
	for _, want := range []string{"registry: /x/PROJECTS.md", "Atlas", "Reloaded."} {
		if !strings.Contains(out, want) {
			t.Errorf("the chat does not show %q after /sync:\n%s", want, out)
		}
	}

	// A sync that ran and found problems shows its report and says so.
	m.opts.Sync = func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, "missing\tGhost\n") //nolint:errcheck
		return errors.New("sync found problems")
	}
	typeAndSend(t, m, "/sync")
	if !strings.Contains(m.status, "problems") || !strings.Contains(m.chat.outVP.View(), "Ghost") {
		t.Errorf("a sync with problems: status = %q", m.status)
	}
}

// TestSlashSyncCanBeStopped: esc stops a sync, and its late result is dropped.
func TestSlashSyncCanBeStopped(t *testing.T) {
	m := chatModel(t, &chatFake{})
	m.opts.Sync = func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, "LATE-REPORT\n") //nolint:errcheck
		return nil
	}
	m.chat.ta.SetValue("/sync")
	cmd := m.send()
	if m.chat.proposing == nil || !strings.Contains(m.chatHint(), "syncing") {
		t.Fatalf("/sync did not start: hint = %q", m.chatHint())
	}
	typeAndSend(t, m, "hello")
	if !strings.Contains(m.status, "still syncing") {
		t.Errorf("a message was accepted mid-sync: status = %q", m.status)
	}
	key(m, "esc")
	m.Update(cmd())
	if strings.Contains(m.chat.outVP.View(), "LATE-REPORT") {
		t.Error("a stopped sync's report still arrived")
	}
}

// TestSyncReportFitsTheFrame: the report is preformatted and truncated rather
// than wrapped, and a sync table is wider than most panes.
func TestSyncReportFitsTheFrame(t *testing.T) {
	row := "ok\tAtlas\tmain@4f2c9e1 clean\tstale (abc -> def)\t" + strings.Repeat("~/Development/deeply/nested/", 5)
	for _, size := range []struct{ w, h int }{{80, 24}, {100, 30}, {60, 20}, {200, 50}, {40, 10}} {
		m := chatModel(t, &chatFake{})
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		m.chat.say(roleReport, "STATE\tPROJECT\tGIT\tDIGEST\tDETAIL\n"+row+"\n"+row)
		m.rerenderChat()
		assertFrameFits(t, m, size.w, size.h, "Chat, sync report")
	}
}
