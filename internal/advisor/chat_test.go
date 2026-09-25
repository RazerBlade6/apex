package advisor

import (
	"context"
	"strings"
	"testing"

	"github.com/RazerBlade6/apex/internal/provider"
)

// TestChatStreamsAndRecordsBothTurns: the sessions and messages tables have
// existed since M1 with nothing writing to them. This is what they were for,
// and a transcript missing half of each exchange is worse than no transcript.
func TestChatStreamsAndRecordsBothTurns(t *testing.T) {
	fake := &fakeProvider{Text: "ScholarRAG is closest.", Usage: provider.Usage{
		InputTokens: 900, OutputTokens: 12, Model: "claude-sonnet-5",
	}}
	h := newHarness(t, fake)
	h.writeIdentity("# Me\n\nI build small tools.\n")
	h.seedProject("scholarrag", "ScholarRAG", "Retrieval over papers.", "hash-1")

	ctx := context.Background()
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chat := h.NewChat(actx)

	events, err := chat.Send(ctx, "Which project is closest to done?")
	if err != nil {
		t.Fatal(err)
	}
	var reply strings.Builder
	var usage provider.Usage
	for ev := range events {
		switch ev.Type {
		case provider.EventTextDelta:
			reply.WriteString(ev.Text)
		case provider.EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		}
	}
	if err := chat.Complete(ctx, reply.String(), usage); err != nil {
		t.Fatal(err)
	}

	if chat.SessionID == "" {
		t.Fatal("no session row was created")
	}
	sess, err := h.Store.GetSession(ctx, chat.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title != "Which project is closest to done?" {
		t.Errorf("session title = %q; a transcript is found again by its first line", sess.Title)
	}

	msgs, err := h.Store.ListMessages(ctx, chat.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("recorded %d message(s), want the question and the answer", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("roles = %q, %q", msgs[0].Role, msgs[1].Role)
	}
	if msgs[1].Model != "claude-sonnet-5" {
		t.Errorf("model = %q, want the serving model rather than the route's alias", msgs[1].Model)
	}
	if msgs[1].OutputTokens == nil || *msgs[1].OutputTokens != 12 {
		t.Errorf("output tokens = %v, want 12", msgs[1].OutputTokens)
	}
}

// TestChatPromptKeepsTheCachePrefixFirst: chat adds its own instructions and a
// history, and both could easily be put where they would invalidate the
// prefix DESIGN.md §8 orders. Identity and digests belong in System; the
// conversation belongs in Messages.
func TestChatPromptKeepsTheCachePrefixFirst(t *testing.T) {
	fake := &fakeProvider{Text: "ok"}
	h := newHarness(t, fake)
	h.writeIdentity("# Me\n\nPROFILE-MARKER\n")
	h.seedProject("scholarrag", "ScholarRAG", "DIGEST-MARKER", "hash-1")

	ctx := context.Background()
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chat := h.NewChat(actx)
	drain(t, chat, ctx, "First question.")
	drain(t, chat, ctx, "Second question.")

	req := fake.Requests[1]
	if !req.CacheSystem {
		t.Error("the cache breakpoint is not set on a chat request")
	}
	iProfile := strings.Index(req.System, "PROFILE-MARKER")
	iDigest := strings.Index(req.System, "DIGEST-MARKER")
	switch {
	case iProfile < 0 || iDigest < 0:
		t.Fatalf("identity or digests missing from System:\n%s", req.System)
	case iProfile > iDigest:
		t.Error("digests precede the identity context; the stable prefix is ordered wrong")
	}
	if strings.Contains(req.System, "First question.") {
		t.Error("conversation history leaked into System, which invalidates the cache prefix every turn")
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want two prior turns plus the new one", len(req.Messages))
	}
	if req.Messages[len(req.Messages)-1].Content != "Second question." {
		t.Errorf("the volatile instruction is not last: %+v", req.Messages)
	}
}

func TestChatHistoryIsBounded(t *testing.T) {
	fake := &fakeProvider{Text: "ok"}
	h := newHarness(t, fake)
	h.seedProject("p", "P", "digest", "hash")

	ctx := context.Background()
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chat := h.NewChat(actx)
	for i := 0; i < chatHistoryTurns; i++ {
		drain(t, chat, ctx, "question")
	}
	last := fake.Requests[len(fake.Requests)-1]
	// The window plus the new instruction. Unbounded history would grow
	// without limit on a transport where DESIGN.md §8 measured the prefix
	// being re-sent at full price every call.
	if len(last.Messages) > chatHistoryTurns+1 {
		t.Fatalf("messages = %d, want at most %d", len(last.Messages), chatHistoryTurns+1)
	}
}

func TestChatDoesNotRecordAnEmptyReply(t *testing.T) {
	fake := &fakeProvider{Text: ""}
	h := newHarness(t, fake)
	h.seedProject("p", "P", "digest", "hash")

	ctx := context.Background()
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chat := h.NewChat(actx)
	if _, err := chat.Send(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := chat.Complete(ctx, "   ", provider.Usage{}); err != nil {
		t.Fatal(err)
	}

	msgs, err := h.Store.ListMessages(ctx, chat.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("recorded %d message(s), want only the question", len(msgs))
	}
	if n := len(chat.History); n != 1 {
		t.Fatalf("history = %d turns; an empty assistant turn would be re-sent on every later call", n)
	}
}

func drain(t *testing.T, chat *Chat, ctx context.Context, input string) {
	t.Helper()
	events, err := chat.Send(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	var usage provider.Usage
	for ev := range events {
		if ev.Type == provider.EventTextDelta {
			b.WriteString(ev.Text)
		}
		if ev.Type == provider.EventDone && ev.Usage != nil {
			usage = *ev.Usage
		}
	}
	if err := chat.Complete(ctx, b.String(), usage); err != nil {
		t.Fatal(err)
	}
}
