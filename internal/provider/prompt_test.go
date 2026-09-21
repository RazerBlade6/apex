package provider

import (
	"strings"
	"testing"
)

func TestPromptOrdersStablePrefixFirst(t *testing.T) {
	p := Prompt{
		Identity: []Section{
			{Title: "Profile", Body: "Vedant builds CLIs."},
			{Title: "Skills", Body: "Go, Rust."},
		},
		Digests: []Section{
			{Title: "Project: apex", Body: "Building the provider layer."},
			{Title: "Project: scholarrag", Body: "Blocked on chunking."},
		},
		Instruction: "What should I work on next?",
	}

	req := p.Build("claude-opus-5", "high", 4096)

	// Identity first, then digests: the cache prefix is ordered by how often
	// each part changes (DESIGN.md §8).
	want := []string{"Profile", "Skills", "Project: apex", "Project: scholarrag"}
	at := make([]int, len(want))
	for i, w := range want {
		at[i] = strings.Index(req.System, w)
		if at[i] < 0 {
			t.Fatalf("System is missing %q:\n%s", w, req.System)
		}
	}
	for i := 1; i < len(at); i++ {
		if at[i] <= at[i-1] {
			t.Errorf("%q appears before %q; identity must precede digests", want[i], want[i-1])
		}
	}

	// The volatile part is not in the cached prefix.
	if strings.Contains(req.System, p.Instruction) {
		t.Error("the instruction leaked into the cached system prefix")
	}
	if len(req.Messages) != 1 || req.Messages[0].Content != p.Instruction {
		t.Errorf("Messages = %+v, want the instruction as the single user turn", req.Messages)
	}
	if req.Messages[0].Role != RoleUser {
		t.Errorf("Role = %q, want %q", req.Messages[0].Role, RoleUser)
	}

	// And the breakpoint is placed, because there is a stable prefix to cache.
	if !req.CacheSystem {
		t.Error("CacheSystem = false; the breakpoint belongs after the digests")
	}
	if req.Model != "claude-opus-5" || req.Effort != "high" || req.MaxTokens != 4096 {
		t.Errorf("route was not applied: %+v", req)
	}
}

func TestPromptBuild(t *testing.T) {
	tests := []struct {
		name          string
		prompt        Prompt
		wantSystem    string
		wantMessages  []Message
		wantBreakpont bool
	}{
		{
			name:          "nothing stable means no breakpoint",
			prompt:        Prompt{Instruction: "hi"},
			wantSystem:    "",
			wantMessages:  []Message{{Role: RoleUser, Content: "hi"}},
			wantBreakpont: false,
		},
		{
			name: "a section with no title emits only its body",
			prompt: Prompt{
				Identity:    []Section{{Body: "bare"}},
				Instruction: "hi",
			},
			wantSystem:    "bare",
			wantMessages:  []Message{{Role: RoleUser, Content: "hi"}},
			wantBreakpont: true,
		},
		{
			name: "empty sections are dropped rather than left as blank headings",
			prompt: Prompt{
				Identity:    []Section{{Title: "Profile", Body: "who"}, {}},
				Instruction: "hi",
			},
			wantSystem:    "# Profile\n\nwho",
			wantMessages:  []Message{{Role: RoleUser, Content: "hi"}},
			wantBreakpont: true,
		},
		{
			name: "history precedes the instruction",
			prompt: Prompt{
				Identity: []Section{{Title: "Profile", Body: "who"}},
				History: []Message{
					{Role: RoleUser, Content: "earlier"},
					{Role: RoleAssistant, Content: "answer"},
				},
				Instruction: "later",
			},
			wantSystem: "# Profile\n\nwho",
			wantMessages: []Message{
				{Role: RoleUser, Content: "earlier"},
				{Role: RoleAssistant, Content: "answer"},
				{Role: RoleUser, Content: "later"},
			},
			wantBreakpont: true,
		},
		{
			name:          "no instruction leaves the history alone",
			prompt:        Prompt{History: []Message{{Role: RoleUser, Content: "only"}}},
			wantSystem:    "",
			wantMessages:  []Message{{Role: RoleUser, Content: "only"}},
			wantBreakpont: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.prompt.Build("claude-opus-5", "low", 100)
			if req.System != tt.wantSystem {
				t.Errorf("System = %q, want %q", req.System, tt.wantSystem)
			}
			if len(req.Messages) != len(tt.wantMessages) {
				t.Fatalf("Messages = %+v, want %+v", req.Messages, tt.wantMessages)
			}
			for i, m := range req.Messages {
				if m != tt.wantMessages[i] {
					t.Errorf("Messages[%d] = %+v, want %+v", i, m, tt.wantMessages[i])
				}
			}
			if req.CacheSystem != tt.wantBreakpont {
				t.Errorf("CacheSystem = %v, want %v", req.CacheSystem, tt.wantBreakpont)
			}
		})
	}
}
