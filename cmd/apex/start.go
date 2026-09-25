package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/executor"
	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/store"
)

// `apex start` is DESIGN.md §14's three tiers, escalating from deterministic
// to generative, on the principle that a model should only do the part that
// genuinely varies.
//
//	tier 1  Apex, deterministic    a directory, git init, PROJECT.md,
//	                               .gitignore, an entry in PROJECTS.md
//	tier 2  the ecosystem          go mod init, cargo init, uv init,
//	                               npm create vite — skipped when nothing fits
//	tier 3  the executor           whatever structure this idea specifically
//	                               needs
//
// One ordering detail departs from the spec's presentation, deliberately.
// Tier 1 creates the directory and then STOPS; tier 2 runs into the empty
// directory; tier 1's file writes follow. The reason is that ecosystem
// scaffolders behave badly in a directory that is not empty — `npm create
// vite` asks whether to delete what is there, on a terminal nobody is
// watching — and an interactive prompt is exactly what unattended dispatch
// cannot survive. The tiers still do what §14 says they do, in the order it
// says; only the file writes move to the far side of tier 2.

// defaultProjectParent is where a new project goes when --path does not say.
const defaultProjectParent = "Development"

func newStartCmd() *cobra.Command {
	var (
		opts       dispatchOptions
		path       string
		name       string
		stack      string
		module     string
		noDispatch bool
	)

	cmd := &cobra.Command{
		Use:   "start <idea-id>",
		Short: "Scaffold a new project from an idea",
		Long: `Turn a proposed idea into a real project: a directory, a git repository, a
PROJECT.md written from the idea, an entry in PROJECTS.md, an idiomatic
skeleton from the ecosystem's own tooling where one fits, and finally a
scaffolding pass by a coding agent.

Three tiers, escalating from deterministic to generative. Apex does the part
that is identical for every project, the ecosystem does the part it maintains
better than any template could, and the agent does the part that is specific
to this idea.

--stack picks the ecosystem scaffolder (` + strings.Join(stackNames(), ", ") + `).
Without it, apex looks for an unambiguous language in the idea's own text and
skips the tier when it finds none. A scaffolder whose tool is not installed is
reported and skipped: the project is still created.

--no-dispatch stops after tier 2, which costs no subscription quota.

Nothing here is committed. The first commit is yours to make once you have
read what was generated.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd.Context(), cmd.OutOrStdout(), args[0], startOptions{
				Dispatch:   opts,
				Path:       path,
				Name:       name,
				Stack:      stack,
				Module:     module,
				NoDispatch: noDispatch,
			})
		},
	}

	cmd.Flags().StringVar(&path, "path", "",
		"where to create the project (default ~/"+defaultProjectParent+"/<name>)")
	cmd.Flags().StringVar(&name, "name", "",
		"project name, if it should differ from the idea's title")
	cmd.Flags().StringVar(&stack, "stack", "",
		"ecosystem scaffolder to run ("+strings.Join(stackNames(), ", ")+")")
	cmd.Flags().StringVar(&module, "module", "",
		"Go module path, for --stack go (default: the project slug)")
	cmd.Flags().BoolVar(&noDispatch, "no-dispatch", false,
		"stop after the ecosystem scaffolder; do not dispatch a scaffolding brief")
	cmd.Flags().StringVar(&opts.Model, "model", "",
		"model for the scaffolding dispatch, overriding executor.model")
	cmd.Flags().StringVar(&opts.Effort, "effort", "",
		"effort for the scaffolding dispatch, overriding executor.effort")
	cmd.Flags().BoolVar(&opts.Bypass, "bypass-permissions", false,
		"grant the agent every permission, not only file edits, so it can build and test its own work")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", defaultDispatchTimeout,
		"stop the dispatch after this long; 0 waits indefinitely")
	return cmd
}

type startOptions struct {
	Dispatch   dispatchOptions
	Path       string
	Name       string
	Stack      string
	Module     string
	NoDispatch bool
}

func runStart(ctx context.Context, w io.Writer, rawID string, opts startOptions) error {
	// start writes to PROJECTS.md and the projects table, so it is a
	// mutating command — but it does not own migrations. sync does
	// (DESIGN.md §18), and a scaffold is not the moment to take a one-way
	// schema door.
	sess, err := openSession(ctx, false)
	if err != nil {
		return err
	}
	defer sess.Close()

	idea, err := loadStartableIdea(ctx, sess, rawID)
	if err != nil {
		return err
	}

	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = idea.Title
	}
	slug := project.Slug(name)
	if slug == "" {
		return &badProjectNameError{Name: name}
	}

	home, err := project.Home()
	if err != nil {
		return err
	}
	dir := strings.TrimSpace(opts.Path)
	if dir == "" {
		dir = filepath.Join(home, defaultProjectParent, name)
	} else if dir, err = project.ExpandPath(dir, home); err != nil {
		return err
	}

	fmt.Fprintf(w, "%s  %s\n", idea.ID, idea.Title)
	fmt.Fprintf(w, "creating %s\n\n", dir)

	// --- tier 1a: the directory --------------------------------------------
	if err := makeProjectDir(dir); err != nil {
		return err
	}

	// --- tier 2: the ecosystem's own scaffolder ----------------------------
	// It runs before the file writes so it meets an empty directory; see the
	// note at the top of this file.
	scaffold := runTierTwo(ctx, w, dir, slug, opts, idea)

	// --- tier 1b: git, PROJECT.md, .gitignore, the registry ----------------
	if err := tierOne(ctx, w, sess, dir, name, slug, idea, scaffold); err != nil {
		return err
	}

	// The idea is now a project, whether or not tier 3 runs. Recording it
	// here rather than after the dispatch means a failed or skipped agent
	// pass does not leave the idea looking unstarted while its directory
	// exists.
	if err := sess.Store.SetIdeaStatus(ctx, idea.ID, store.IdeaStarted, slug); err != nil {
		return err
	}

	// --- tier 3: the executor ----------------------------------------------
	if opts.NoDispatch {
		fmt.Fprintln(w, "\n--no-dispatch: no scaffolding brief was sent to an agent.")
		writeStartNextSteps(w, dir, name)
		return nil
	}

	exec, err := newExecutor(sess.Config, opts.Dispatch)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "\nDispatching a scaffolding brief...")
	if _, err := runDispatch(ctx, w, sess, exec, dispatchSpec{
		ProjectSlug: slug,
		ProjectPath: dir,
		ProjectName: name,
		Title:       "Scaffold " + name,
		Intent:      scaffoldIntent(name, idea, scaffold),
		Bypass:      opts.Dispatch.Bypass,
	}); err != nil {
		// The project exists and is registered. A failed scaffolding pass is
		// worth reporting and is not worth unwinding tier 1 over.
		fmt.Fprintf(w, "\nThe scaffolding dispatch failed. The project itself is created and registered.\n")
		return err
	}
	writeStartNextSteps(w, dir, name)
	return nil
}

// runTierTwo chooses and runs the ecosystem scaffolder, reporting either way.
func runTierTwo(ctx context.Context, w io.Writer, dir, slug string, opts startOptions, idea store.Idea) scaffoldResult {
	stack := strings.TrimSpace(opts.Stack)
	if stack == "" {
		inferred, token, ok := inferStack(idea.Title + " " + idea.Pitch + " " + idea.Rationale)
		if !ok {
			fmt.Fprintf(w, "No stack named in the idea, so no ecosystem scaffolder ran.\n")
			fmt.Fprintf(w, "  --stack %s   to pick one\n", strings.Join(stackNames(), "|"))
			return scaffoldResult{Detail: "no stack was named or inferred"}
		}
		stack = inferred
		fmt.Fprintf(w, "Stack %q inferred from %q in the idea. --stack overrides it.\n", stack, token)
	}

	module := strings.TrimSpace(opts.Module)
	if module == "" {
		module = slug
	}

	result := runScaffolder(ctx, dir, stack, slug, module)
	switch {
	case result.Err != nil:
		fmt.Fprintf(w, "%s failed: %s\n", result.Command, result.Detail)
		fmt.Fprintln(w, "  the project is still created; run the scaffolder yourself if you want it")
	case result.Ran:
		fmt.Fprintf(w, "%s\n", result.Command)
	default:
		fmt.Fprintf(w, "%s\n", result.Detail)
	}
	return result
}

// tierOne is the deterministic half: git, the two files, and the registry.
func tierOne(ctx context.Context, w io.Writer, sess *session, dir, name, slug string, idea store.Idea, scaffold scaffoldResult) error {
	// git init, if git is here at all. Apex must never assume a binary is
	// present — the same rule Available() enforces for the executor.
	switch {
	case isGitRepo(dir):
		fmt.Fprintln(w, "git repository already initialised by the scaffolder")
	default:
		if err := gitInitDir(ctx, dir); err != nil {
			fmt.Fprintf(w, "git init skipped: %v\n", err)
		} else {
			fmt.Fprintln(w, "git init")
		}
	}

	docPath := contextfs.ProjectDocPath(dir)
	if err := writeIfAbsent(docPath, projectDocFor(name, idea)); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote %s\n", contextfs.ProjectFile)

	if wrote, err := writeGitignore(dir, scaffold.Ignore); err != nil {
		return err
	} else if wrote {
		fmt.Fprintln(w, "wrote .gitignore")
	} else {
		fmt.Fprintln(w, ".gitignore left as the scaffolder wrote it")
	}

	// The registry. PROJECTS.md is the only file that answers "which projects
	// exist and where do they live" (DESIGN.md §6), so a project Apex created
	// and did not register would be invisible to every other command.
	registryPath := contextfs.RegistryPath(sess.Root)
	reg, err := contextfs.LoadRegistry(ctx, registryPath)
	if err != nil {
		return err
	}
	if _, exists := reg.Lookup(name); !exists {
		if err := reg.AddEntry(name, tildify(dir, mustHome())); err != nil {
			return err
		}
		if err := reg.Save(ctx); err != nil {
			return err
		}
		fmt.Fprintf(w, "registered in %s\n", registryPath)
	}

	// And the row, so `apex items` and `apex review` can see it before the
	// next sync. The digest still comes from sync; this is registration, not
	// summarisation.
	candidate := project.Candidate{Slug: slug, Name: name, RawPath: dir, Path: dir}
	if _, err := project.Upsert(ctx, sess.Store, candidate, "active", time.Now()); err != nil {
		return err
	}
	return nil
}

// scaffoldIntent is tier 3's brief.
//
// It carries the idea — which exists nowhere in the working tree except in the
// PROJECT.md tier 1 just wrote — plus what tiers 1 and 2 already did, so the
// agent extends a skeleton rather than recreating one.
func scaffoldIntent(name string, idea store.Idea, scaffold scaffoldResult) executor.Intent {
	constraints := []string{
		"This is a new, empty project. Build the smallest structure that makes the first milestone possible, and stop there.",
		"Do not add dependencies beyond what that structure needs, and do not scaffold features the pitch does not ask for.",
	}
	if scaffold.Ran && scaffold.Err == nil {
		constraints = append(constraints,
			fmt.Sprintf("`%s` has already run in this directory. Extend what it produced; do not replace or duplicate it.", scaffold.Command))
	}

	return executor.Intent{
		Kind:        "new project",
		ID:          idea.ID,
		ProjectName: name,
		Title:       "Scaffold " + name + " so the first milestone can begin",
		What:        idea.Pitch,
		Why:         idea.Rationale,
		Effort:      "small",
		Acceptance: []string{
			"the project builds and runs, even if it does nothing useful yet",
			"a README explains what this is and how to run it",
			"PROJECT.md is left alone: apex owns that file",
			"no placeholder code that pretends to work — a TODO is better than a stub that lies",
		},
		ContextPaths: []string{contextfs.ProjectFile},
		Constraints:  constraints,
	}
}

// projectDocFor renders the PROJECT.md a new project starts with (DESIGN.md
// §6's format), written from the idea so the file says something real on day
// one rather than being an empty template.
func projectDocFor(name string, idea store.Idea) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	b.WriteString("status: active\n")
	b.WriteString("stack: []\n")
	fmt.Fprintf(&b, "started: %s\n", time.Now().Format("2006-01-02"))
	fmt.Fprintf(&b, "idea: %s\n", idea.ID)
	b.WriteString("---\n\n")

	b.WriteString("## What it is\n\n")
	if pitch := strings.TrimSpace(idea.Pitch); pitch != "" {
		b.WriteString(pitch + "\n\n")
	} else {
		b.WriteString(strings.TrimSpace(idea.Title) + "\n\n")
	}

	b.WriteString("## Current state\n\n")
	b.WriteString("Just scaffolded. Nothing works yet.\n\n")

	b.WriteString("## Why this project\n\n")
	if rationale := strings.TrimSpace(idea.Rationale); rationale != "" {
		b.WriteString(rationale + "\n\n")
	} else {
		b.WriteString("Recorded from " + idea.ID + ".\n\n")
	}

	b.WriteString("## Goals\n\n")
	b.WriteString("- Reach the first milestone described above\n")
	return b.String()
}

// baseGitignore is what every project gets regardless of stack: the files a
// mac and an editor leave behind, and the two directories that are never
// wanted in a repository.
var baseGitignore = []string{
	".DS_Store",
	".env",
	".env.local",
	"*.log",
	".idea/",
	".vscode/",
}

// writeGitignore adds Apex's baseline plus any stack extras, and reports
// whether it wrote anything. A .gitignore the scaffolder already wrote is left
// alone: cargo and vite know their own ecosystems better than this list does.
func writeGitignore(dir string, extra []string) (bool, error) {
	path := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}

	lines := append([]string{}, baseGitignore...)
	lines = append(lines, extra...)
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// writeIfAbsent writes a file only when there is nothing there, so a rerun
// never overwrites what a user has since edited.
func writeIfAbsent(path, body string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// makeProjectDir creates the project directory, refusing to scaffold into a
// directory that already has something in it.
//
// The refusal is the point. `apex start` runs an ecosystem scaffolder and then
// an agent with edit permissions; pointing both at a directory that already
// holds work is how a mistyped --path becomes a bad afternoon.
func makeProjectDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read %s: %w", dir, err)
	case len(entries) > 0:
		return &occupiedDirError{Path: dir, Entries: len(entries)}
	default:
		return nil
	}
}

func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

// gitInitDir runs `git init`, checking first that git is actually here.
func gitInitDir(ctx context.Context, dir string) error {
	bin, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git is not on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "init", "--quiet")
	cmd.Dir = dir
	cmd.Stdin = nil
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %s", lastLines(string(out), 3))
	}
	return nil
}

// loadStartableIdea resolves the id and refuses an idea that is already a
// project or was dismissed.
func loadStartableIdea(ctx context.Context, sess *session, rawID string) (store.Idea, error) {
	_, ideaID := normalizeID(rawID)
	if ideaID == "" {
		return store.Idea{}, &unknownIDError{ID: rawID}
	}
	idea, err := sess.Store.GetIdea(ctx, ideaID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Idea{}, &unknownIDError{ID: rawID}
	}
	if err != nil {
		return store.Idea{}, err
	}
	switch idea.Status {
	case store.IdeaStarted:
		return store.Idea{}, &alreadyStartedError{ID: idea.ID, Slug: idea.StartedProjectSlug}
	case store.IdeaDismissed:
		return store.Idea{}, &dismissedIdeaError{ID: idea.ID}
	}
	return idea, nil
}

func writeStartNextSteps(w io.Writer, dir, name string) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  cd %s\n", dir)
	fmt.Fprintln(w, "  git status            nothing is committed; the first commit is yours")
	fmt.Fprintf(w, "  apex sync %s   to generate its digest\n", name)
}

// mustHome returns the home directory, or "" when it cannot be determined. A
// blank home only means a registry path is written absolute rather than as
// ~/..., which is cosmetic.
func mustHome() string {
	home, err := project.Home()
	if err != nil {
		return ""
	}
	return home
}

type occupiedDirError struct {
	Path    string
	Entries int
}

func (e *occupiedDirError) Error() string {
	return fmt.Sprintf("%s already exists and is not empty (%d entries)\n"+
		"  apex start scaffolds into an empty directory and will not write over existing work\n"+
		"  pass --path to choose another location", e.Path, e.Entries)
}

func (e *occupiedDirError) UserFixable() bool { return true }

type alreadyStartedError struct{ ID, Slug string }

func (e *alreadyStartedError) Error() string {
	msg := fmt.Sprintf("%s has already been started", e.ID)
	if e.Slug != "" {
		msg += fmt.Sprintf(" as project %q", e.Slug)
	}
	return msg + "\n  apex items   for the work outstanding on it"
}

func (e *alreadyStartedError) UserFixable() bool { return true }

type dismissedIdeaError struct{ ID string }

func (e *dismissedIdeaError) Error() string {
	return fmt.Sprintf("%s was dismissed\n  apex ideas   to propose new ones", e.ID)
}

func (e *dismissedIdeaError) UserFixable() bool { return true }

type badProjectNameError struct{ Name string }

func (e *badProjectNameError) Error() string {
	return fmt.Sprintf("%q has no letters or digits, so it cannot name a project\n"+
		"  pass --name to give it one", e.Name)
}

func (e *badProjectNameError) UserFixable() bool { return true }
