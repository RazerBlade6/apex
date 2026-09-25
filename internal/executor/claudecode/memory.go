package claudecode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RazerBlade6/apex/internal/executor"
)

// Excluding the user's global CLAUDE.md (DESIGN.md §9).
//
// # The decision
//
// A dispatch inherits the *project* CLAUDE.md and not the user's global one.
// Project inheritance is the design, and it is why the brief can stay short.
// Global inheritance was a side effect nobody chose: both real M5 dispatches
// volunteered that they were departing from a convention in ~/.claude/CLAUDE.md,
// meaning personal Claude Code conventions silently shaped every Apex run.
//
// # The mechanism, settled empirically in M6
//
// §9 listed three remaining approaches in preference order. The first was
// tried and fails; the second works and is what ships.
//
// **Scoping the config directory — rejected, and cheaply.** Setting
// CLAUDE_CONFIG_DIR does relocate CLAUDE.md discovery, but it also relocates
// .claude.json, and with it the logged-in account. Against claude 2.1.282:
//
//	$ CLAUDE_CONFIG_DIR=/tmp/scoped claude auth status --json
//	{"loggedIn": false, "authMethod": "none", ...}
//
// against {"loggedIn": true, "authMethod": "claude.ai", "subscriptionType":
// "pro"} with the real directory. Seeding the scoped directory with the real
// oauthAccount did not change it. This is the same failure as --bare, arrived
// at by a different route: it disables the mechanism this executor exists to
// use. `claude auth status --json` settles it without spending a single token,
// which is worth remembering the next time a flag needs checking.
//
// **--safe-mode plus re-supplying the project CLAUDE.md — adopted.** Verified
// by dispatch against claude 2.1.282, in a fixture project whose CLAUDE.md
// said to end every reply with a specific word. The run authenticated, kept
// Bash, Read, Write and Edit, followed the re-supplied project convention, and
// answered "no" when asked whether it had been told anything about delegating
// work to a sub-agent — which is the rule in the real global file. Global
// excluded, project honoured, auth intact.
//
// # What this costs, stated plainly
//
// --safe-mode disables *all* customizations, so a dispatch also loses the
// user's skills, hooks, plugins, MCP servers, output styles and settings.json
// — at both user AND project scope. Apex re-supplies exactly one of those: the
// project's root CLAUDE.md, inlined into the appended system prompt under a
// heading that says where it came from.
//
// That is deliberately narrower than "inherit the project's configuration",
// and §9 demands the difference be documented rather than hidden: "a dispatch
// that silently inherits half a config is worse than one that documents what
// it inherits." So, precisely, a dispatch inherits:
//
//   - the project's root CLAUDE.md, re-supplied by Apex;
//   - everything in the working tree, because the agent is standing in it.
//
// and does not inherit:
//
//   - the user's global CLAUDE.md — the point of the exercise;
//   - CLAUDE.md files in subdirectories, and @-imports from any of them;
//   - skills, hooks, plugins, MCP servers and settings.json at either scope.
//
// A user who wants their own configuration back sets
// `executor.inherit_user_config = true`, which drops --safe-mode and restores
// the 2.1.x default behaviour, global CLAUDE.md included.

// ProjectMemoryHeading introduces the re-supplied CLAUDE.md in the brief.
const ProjectMemoryHeading = "## This project's CLAUDE.md"

// projectMemoryNote tells the agent why it is reading the file in its prompt
// rather than discovering it. Without this the agent sees the same content
// twice — once as a section of a system prompt, once as a file in the
// directory it was told to read first — and has no way to know they are the
// same thing.
const projectMemoryNote = "Re-supplied verbatim by Apex because this run starts with customizations " +
	"disabled, so that your own global conventions do not leak into it. These are the project's " +
	"conventions and they outrank your defaults."

// projectMemoryLimit bounds how much of a CLAUDE.md is inlined. A file past
// this is a document rather than a set of conventions, and the agent can read
// the rest from the working tree.
const projectMemoryLimit = 24000

// readProjectMemory returns the project's root CLAUDE.md, or "" when there is
// none.
//
// Only the root file, and only for the project. Nested CLAUDE.md files and
// @-imports are not followed: resolving them would mean reimplementing the
// CLI's own discovery rules against a moving target, and getting that subtly
// wrong is precisely the "half a config" outcome §9 rules out. The limitation
// is documented above instead.
func readProjectMemory(dir string) (string, error) {
	path := filepath.Join(dir, "CLAUDE.md")
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("executor: read %s: %w", path, err)
	}
	body := strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if len(body) > projectMemoryLimit {
		body = body[:projectMemoryLimit] + "\n\n[truncated by apex]"
	}
	return body, nil
}

// systemPrompt assembles what goes on --append-system-prompt: the brief, then
// the project's own CLAUDE.md when one is being re-supplied.
//
// The brief comes first because it is what this run is for. The project's
// conventions come after it, labelled, so the agent can tell Apex's intent
// from the project's standing rules — two things that would be indistinguishable
// if they were concatenated silently.
func systemPrompt(brief executor.TaskBrief, projectMemory string) string {
	brief.Instruction = strings.TrimRight(brief.Instruction, "\n")
	if strings.TrimSpace(projectMemory) == "" {
		return brief.Instruction
	}
	var b strings.Builder
	b.WriteString(brief.Instruction)
	b.WriteString("\n\n")
	b.WriteString(ProjectMemoryHeading)
	b.WriteString("\n\n")
	b.WriteString(projectMemoryNote)
	b.WriteString("\n\n")
	b.WriteString(projectMemory)
	b.WriteString("\n")
	return b.String()
}
