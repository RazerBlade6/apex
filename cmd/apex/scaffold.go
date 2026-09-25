package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Tier 2 of `apex start` (DESIGN.md §14): the ecosystem's own scaffolder.
//
// The argument for this tier is that `go mod init`, `cargo init` and `uv init`
// are maintained by the people who define what idiomatic layout means, and
// they are current in a way neither a template Apex ships nor a model's
// recollection can be. Apex keeps the mapping and nothing else, and skips the
// tier entirely when nothing matches.
//
// Two rules make it safe to run unattended, and both come from the same place
// as the executor's --permission-prompts none: nothing is watching a TTY.
// Every scaffolder runs with no stdin and under a timeout, so one that decides
// to ask a question fails with an answer rather than hanging forever.

// scaffoldTimeout bounds one scaffolder. `npm create vite` downloads a package
// before it does anything, so this is generous; it is a guard against a hang,
// not a performance budget.
const scaffoldTimeout = 3 * time.Minute

// scaffolder is one ecosystem's project initialiser.
type scaffolder struct {
	// Stack is the canonical name, as `--stack` takes it.
	Stack string
	// Bin is the executable that must exist for this tier to run at all.
	Bin string
	// Args builds the command line. slug is the project's slug and module is
	// the Go module path, which nothing else uses.
	Args func(slug, module string) []string
	// Note explains what the scaffolder did, for the report.
	Note string
	// Ignore is appended to .gitignore for this stack when the scaffolder
	// did not write one itself.
	Ignore []string
}

// scaffolders is the whole of Apex's knowledge about ecosystem tooling. It is
// deliberately short: a stack not listed here gets tiers 1 and 3, which is a
// working project, just not an idiomatic skeleton.
var scaffolders = []scaffolder{
	{
		Stack: "go",
		Bin:   "go",
		Args:  func(slug, module string) []string { return []string{"mod", "init", module} },
		Note:  "go mod init",
		// go.sum and vendor/ are not ignored: both are meant to be
		// committed. The binary a build drops in the module root is not.
		Ignore: []string{"/" + "dist/", "*.test", "*.out"},
	},
	{
		Stack: "rust",
		Bin:   "cargo",
		// init rather than new: tier 1 has already created the directory,
		// and `cargo new` would insist on creating one of its own.
		Args:   func(slug, module string) []string { return []string{"init", "--name", slug} },
		Note:   "cargo init",
		Ignore: nil, // cargo writes its own .gitignore
	},
	{
		Stack:  "python",
		Bin:    "uv",
		Args:   func(slug, module string) []string { return []string{"init", "--name", slug} },
		Note:   "uv init",
		Ignore: nil, // uv writes its own .gitignore
	},
	{
		Stack: "node",
		Bin:   "npm",
		// -y answers npm's "install the package?" prompt, and --template
		// answers vite's framework question. Without both, this hangs on a
		// terminal nobody is reading.
		Args: func(slug, module string) []string {
			return []string{"create", "-y", "vite@latest", ".", "--", "--template", "vanilla-ts"}
		},
		Note:   "npm create vite",
		Ignore: nil, // vite's template writes its own .gitignore
	},
}

func scaffolderFor(stack string) (scaffolder, bool) {
	stack = strings.ToLower(strings.TrimSpace(stack))
	for _, s := range scaffolders {
		if s.Stack == stack {
			return s, true
		}
	}
	return scaffolder{}, false
}

// stackNames lists what --stack accepts, for help and error messages.
func stackNames() []string {
	out := make([]string, 0, len(scaffolders))
	for _, s := range scaffolders {
		out = append(out, s.Stack)
	}
	return out
}

// stackTokens maps text an idea might contain onto a stack.
//
// Every token is unambiguous in English prose, which is why "go" is not one of
// them: an idea whose pitch says "a tool to go through your inbox" must not
// scaffold a Go module. The cost of being conservative is that the tier is
// skipped and the user passes --stack; the cost of being clever is a project
// scaffolded in the wrong language, which is worse and quieter.
var stackTokens = []struct {
	token string
	stack string
}{
	{"golang", "go"},
	{"go module", "go"},
	{"go cli", "go"},
	{"in go", "go"},
	{"rust", "rust"},
	{"cargo", "rust"},
	{"python", "python"},
	{"fastapi", "python"},
	{"django", "python"},
	{"typescript", "node"},
	{"javascript", "node"},
	{"react", "node"},
	{"vite", "node"},
	{"node.js", "node"},
}

// inferStack guesses a stack from the idea's own words, reporting the token
// that decided it so the user can see why.
func inferStack(text string) (stack, token string, ok bool) {
	lower := strings.ToLower(text)
	for _, t := range stackTokens {
		if strings.Contains(lower, t.token) {
			return t.stack, t.token, true
		}
	}
	return "", "", false
}

// scaffoldResult is what tier 2 did, or why it did nothing.
type scaffoldResult struct {
	// Stack is the stack that was chosen, empty when none was.
	Stack string
	// Ran is true when a scaffolder actually executed.
	Ran bool
	// Command is what was run, for the report.
	Command string
	// Detail explains the outcome: the skip reason, or the tool's own error.
	Detail string
	// Ignore is what the caller should add to .gitignore, empty when the
	// scaffolder wrote its own.
	Ignore []string
	// Err is set when the scaffolder ran and failed. It is reported and does
	// not abort `apex start`: tier 1 already produced a real project.
	Err error
}

// runScaffolder runs tier 2 in dir.
//
// A missing binary is a skip, not a failure. This machine has Go and may not
// have uv, cargo or npm, and a start that refused to finish because an
// ecosystem tool was absent would be worse than one that says so and carries
// on with a directory, a git repo and a PROJECT.md.
func runScaffolder(ctx context.Context, dir, stack, slug, module string) scaffoldResult {
	s, ok := scaffolderFor(stack)
	if !ok {
		return scaffoldResult{
			Detail: fmt.Sprintf("no scaffolder for stack %q (known: %s)",
				stack, strings.Join(stackNames(), ", ")),
		}
	}

	bin, err := exec.LookPath(s.Bin)
	if err != nil {
		return scaffoldResult{
			Stack:  s.Stack,
			Detail: fmt.Sprintf("%s is not installed, so %s was skipped", s.Bin, s.Note),
			Ignore: s.Ignore,
		}
	}

	args := s.Args(slug, module)
	ctx, cancel := context.WithTimeout(ctx, scaffoldTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// No stdin: a scaffolder that decided to ask a question must fail rather
	// than wait for an answer nobody will type.
	cmd.Stdin = nil
	out, runErr := cmd.CombinedOutput()

	result := scaffoldResult{
		Stack:   s.Stack,
		Ran:     true,
		Command: s.Bin + " " + strings.Join(args, " "),
		Ignore:  s.Ignore,
	}
	if runErr != nil {
		result.Err = runErr
		result.Detail = strings.TrimSpace(lastLines(string(out), 5))
		if result.Detail == "" {
			result.Detail = runErr.Error()
		}
		return result
	}
	result.Detail = s.Note + " completed"
	return result
}

// lastLines keeps the tail of a command's output, which is where the
// explanation of a failure is.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
