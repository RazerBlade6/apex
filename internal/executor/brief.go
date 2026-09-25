package executor

import (
	"fmt"
	"strings"
)

// Intent is what Apex knows about a task that the working tree does not.
//
// The split is the whole of DESIGN.md §9's brief design. The agent runs
// inside the project, so it reads PROJECT.md, README.md and any CLAUDE.md for
// itself — what the project is, what it uses, how it is built. What it cannot
// read is Apex's judgment: that this item is worth doing now, why it was
// proposed, and what would count as finished. That is what travels in the
// brief, and nothing else does.
//
// The temptation is to paste the digest in "for context". Doing so costs
// tokens to restate what is already on disk, and — worse — ships a summary
// that was generated at some earlier sync as though it were current. The
// file in the working tree is never stale in that way.
type Intent struct {
	// Kind names the sort of work, for the brief's opening line: "action
	// item" or "new project".
	Kind string
	// ID is the action item or idea identifier (AI-014, IDEA-007), so the
	// agent can refer to it and a log reader can correlate.
	ID string
	// ProjectName is the project's name in the registry. It is a label, not
	// background: the agent is already standing in the directory.
	ProjectName string
	// Title is the one-line imperative statement of the task.
	Title string
	// What expands on the title: the action item's body.
	What string
	// Why is the rationale Apex recorded — why this matters now.
	Why string
	// Effort is Apex's size estimate (small | medium | large), which tells
	// the agent how much change is proportionate.
	Effort string
	// Acceptance lists what would make this done. Empty falls back to
	// DefaultAcceptance.
	Acceptance []string
	// ContextPaths names files worth reading first, relative to the project
	// directory. They are pointers; their contents are not inlined.
	ContextPaths []string
	// Constraints are extra rules for this one dispatch.
	Constraints []string
}

// DefaultAcceptance is what "done" means when Apex has nothing more specific.
//
// It is deliberately about evidence rather than effort: a task the agent
// believes it completed but cannot demonstrate is the failure mode that makes
// unattended dispatch untrustworthy.
var DefaultAcceptance = []string{
	"the change builds, and the project's existing tests still pass",
	"the diff is limited to this task — no unrelated refactoring, formatting, or dependency changes",
}

// standingRules are appended to every brief.
//
// Two of them exist for reasons DESIGN.md states outright. Apex never marks an
// item done (§14, step 6) — that is the user's judgment after reading the
// diff — so the agent is told not to pre-empt it. And the work is left
// uncommitted because the diff *is* the review surface: a run that committed
// its own changes would hand the user a merge to undo instead of a change to
// read.
var standingRules = []string{
	"Read PROJECT.md, README.md and any CLAUDE.md in this directory first. They are the project's own account of itself and they outrank anything you infer from the code.",
	"Follow the conventions already in this codebase over your own defaults.",
	"Do not commit, stage, push, or amend. Leave every change in the working tree: the diff is how the user reviews this work.",
	"Do not mark the task complete anywhere, and do not edit Apex's own state. Whether this is finished is the user's call after reading the diff.",
	"If the task turns out to be wrong, already done, or impossible as described, stop and say so plainly. A short explanation of why is worth more than a plausible-looking change.",
	"Finish by summarising what you changed, file by file, and what you did not do.",
}

// Render assembles the brief that is passed to the agent as an appended
// system prompt.
//
// It is markdown because the agent reads markdown all day, and it leads with
// the task rather than with Apex, because the first thing in a system prompt
// is what gets weighted.
func (i Intent) Render() string {
	var b strings.Builder

	kind := strings.TrimSpace(i.Kind)
	if kind == "" {
		kind = "task"
	}
	b.WriteString("# Apex dispatch\n\n")
	fmt.Fprintf(&b, "You are implementing one %s in a project that Apex manages.\n", kind)
	b.WriteString("Apex tracks the work; it does not write the code. You do.\n\n")

	b.WriteString("## The task\n\n")
	if id := strings.TrimSpace(i.ID); id != "" {
		fmt.Fprintf(&b, "**%s", id)
		if name := strings.TrimSpace(i.ProjectName); name != "" {
			fmt.Fprintf(&b, " · %s", name)
		}
		b.WriteString("**\n\n")
	} else if name := strings.TrimSpace(i.ProjectName); name != "" {
		fmt.Fprintf(&b, "**%s**\n\n", name)
	}
	if title := strings.TrimSpace(i.Title); title != "" {
		fmt.Fprintf(&b, "%s\n\n", title)
	}
	if what := strings.TrimSpace(i.What); what != "" {
		fmt.Fprintf(&b, "%s\n\n", what)
	}
	if effort := strings.TrimSpace(i.Effort); effort != "" {
		fmt.Fprintf(&b, "Apex sized this as **%s**. Keep the change proportionate to that.\n\n", effort)
	}

	if why := strings.TrimSpace(i.Why); why != "" {
		b.WriteString("## Why this, now\n\n")
		fmt.Fprintf(&b, "%s\n\n", why)
	}

	acceptance := i.Acceptance
	if len(acceptance) == 0 {
		acceptance = DefaultAcceptance
	}
	b.WriteString("## Done means\n\n")
	for _, line := range acceptance {
		if line = strings.TrimSpace(line); line != "" {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	b.WriteString("\n")

	if len(i.ContextPaths) > 0 {
		b.WriteString("## Read these first\n\n")
		for _, p := range i.ContextPaths {
			if p = strings.TrimSpace(p); p != "" {
				fmt.Fprintf(&b, "- %s\n", p)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## Rules\n\n")
	for _, rule := range standingRules {
		fmt.Fprintf(&b, "- %s\n", rule)
	}
	for _, rule := range i.Constraints {
		if rule = strings.TrimSpace(rule); rule != "" {
			fmt.Fprintf(&b, "- %s\n", rule)
		}
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}
