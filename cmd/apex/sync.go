package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/lock"
	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// `apex sync` is DESIGN.md §14 in full. M2 built steps 1, 2, 3 and 5; M4 adds
// step 4, the one that costs money: for every project whose source hash moved,
// assemble its sources and generate a digest through the digest route, bounded
// by an errgroup.
//
// sync is also the command that owns migrations. `apex doctor` used to apply
// them as a side effect of checking the schema, which DESIGN.md §18 called out
// as a verification command making a one-way change; it now reports and this
// applies.

// syncState is what happened to one project.
type syncState int

const (
	stateOK syncState = iota
	stateBusy
	stateMissing
	stateError
)

func (s syncState) String() string {
	switch s {
	case stateBusy:
		return "busy"
	case stateMissing:
		return "missing"
	case stateError:
		return "ERROR"
	default:
		return "ok"
	}
}

// syncRow is one line of the report, in the style of `apex doctor`.
type syncRow struct {
	Name  string
	State syncState
	Git   string
	// Digest is the rendered freshness cell, and Freshness is the value it was
	// rendered from. The report decides what to regenerate from the value, not
	// from the text of the cell.
	Digest    string
	Freshness project.Freshness
	Detail    string
}

// needsDigest reports whether this project would have a digest generated.
func (r syncRow) needsDigest() bool {
	return r.State == stateOK && r.Freshness.NeedsRegeneration()
}

type syncReport struct {
	rows     []syncRow
	problems []contextfs.Problem
	// regenerate names the projects whose digests are out of date.
	regenerate []string
	// digests is what generation actually produced, one entry per stale
	// project. Empty on a dry run.
	digests []advisor.DigestResult
	// dryRun records that generation was deliberately skipped.
	dryRun bool
	// missingIdentity names the identity documents that do not exist, so the
	// user knows the digests were written without them.
	missingIdentity []string
	// digestModel is the route the digests were generated through.
	digestModel string
	// orphans are registered projects no registry entry points at. They are
	// reported and never removed.
	orphans []project.Orphan
	// rewritten is true when PROJECTS.md was actually rewritten.
	rewritten bool
	// registryPath is reported so the user knows which file was read.
	registryPath string
	// contextRoot is the Apex state root, for naming the directory the
	// identity documents are missing from.
	contextRoot string
}

func (r *syncReport) failed() bool {
	for _, row := range r.rows {
		if row.State == stateError || row.State == stateMissing {
			return true
		}
	}
	for _, d := range r.digests {
		if d.Err != nil {
			return true
		}
	}
	return false
}

func (r *syncReport) write(w io.Writer) error {
	fmt.Fprintf(w, "registry: %s\n\n", r.registryPath)

	if len(r.rows) == 0 {
		fmt.Fprintln(w, "No projects to sync.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "STATE\tPROJECT\tGIT\tDIGEST\tDETAIL")
		for _, row := range r.rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				row.State, row.Name, dash(row.Git), dash(row.Digest), fold(row.Detail))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(r.problems) > 0 {
		fmt.Fprintln(w, "\nRegistry entries skipped (left untouched in the file):")
		for _, p := range r.problems {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}

	if len(r.orphans) > 0 {
		fmt.Fprintln(w, "\nRegistered projects no longer in PROJECTS.md (reported, NOT removed):")
		for _, o := range r.orphans {
			fmt.Fprintf(w, "  %s (%s)\n", o.Name, o.Path)
		}
		fmt.Fprintln(w, "  re-add the entry, or remove the row deliberately: removing it")
		fmt.Fprintln(w, "  destroys its cached digest and every action item keyed to it.")
	}

	fmt.Fprintln(w)
	if r.rewritten {
		fmt.Fprintln(w, "Rewrote the generated summary lines in PROJECTS.md.")
	} else {
		fmt.Fprintln(w, "PROJECTS.md is unchanged.")
	}

	if len(r.regenerate) == 0 {
		fmt.Fprintln(w, "No digests are out of date.")
		return nil
	}
	if r.dryRun {
		fmt.Fprintf(w, "%d digest(s) would be regenerated: %s\n",
			len(r.regenerate), strings.Join(r.regenerate, ", "))
		fmt.Fprintln(w, "--dry-run: nothing was sent to a model.")
		return nil
	}

	if len(r.missingIdentity) > 0 {
		fmt.Fprintf(w, "\nNo %s in %s.\n",
			strings.Join(r.missingIdentity, " or "), contextfs.ContextDir(r.contextRoot))
		fmt.Fprintln(w, "Digests were generated from each project's own sources only. Writing those")
		fmt.Fprintln(w, "files is what makes everything downstream specific to you.")
	}

	var ok, failed int
	for _, d := range r.digests {
		if d.Err != nil {
			failed++
			continue
		}
		ok++
	}
	fmt.Fprintf(w, "\nGenerated %d digest(s) through %s.\n", ok, r.digestModel)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATE\tPROJECT\tMODEL\tTOKENS\tDETAIL")
	for _, d := range r.digests {
		if d.Err != nil {
			fmt.Fprintf(tw, "ERROR\t%s\t-\t-\t%s\n", d.Name, fold(d.Err.Error()))
			continue
		}
		fmt.Fprintf(tw, "ok\t%s\t%s\t%s\t%s\n",
			d.Name, d.Digest.Model, describeUsage(d.Usage), fold(firstSentence(d.Digest.Body)))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if failed > 0 {
		fmt.Fprintf(w, "\n%d digest(s) failed; those projects keep whatever was cached before.\n", failed)
	}
	return nil
}

// describeUsage renders one call's accounting.
//
// Cache writes are shown next to reads deliberately. DESIGN.md §8 argues that
// reads alone cannot tell working caching from a prefix unstable enough to be
// re-written every call, and a sync is exactly where that would show up: the
// digest prompts share a system prefix by construction, so a run whose every
// line reads "w=<large> r=0" means the prefix is not actually stable.
func describeUsage(u provider.Usage) string {
	out := fmt.Sprintf("in=%d out=%d", u.InputTokens, u.OutputTokens)
	if u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
		out += fmt.Sprintf(" cache r=%d w=%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
	return out
}

// firstSentence is a glance at a generated digest, so the table shows that
// something plausible came back without printing three paragraphs per project.
func firstSentence(body string) string {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\n", " "))
	if i := strings.Index(body, ". "); i > 0 && i < 140 {
		return body[:i+1]
	}
	if len(body) > 140 {
		if j := strings.LastIndexByte(body[:140], ' '); j > 0 {
			return body[:j] + "…"
		}
		return body[:140] + "…"
	}
	return body
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// fold keeps a multi-line detail from breaking the table.
func fold(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "; ")
}

func newSyncCmd() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "sync [project]",
		Short: "Refresh the project registry and regenerate stale digests",
		Long: `Read PROJECTS.md, register every project it names, inspect each one's git
state, and regenerate the cached digest of every project whose sources have
changed since the last sync.

A digest is cached against a hash of its sources, so a project that has not
moved costs nothing: only the changed ones reach a model. Generation runs in
parallel, bounded by sync.digest_workers in config.toml, which defaults to a
deliberately small number because a claude-cli route is one subprocess per
project and its session window is shared with your own Claude Code usage.

--dry-run reports what would be regenerated and sends nothing to a model.

sync is also what applies pending database migrations. apex doctor reports
them and does not apply them: a migration is forward-only and cannot be rolled
back, so it happens when you asked for it.

Each project is read under a SHARED lock, so several syncs may run at once but
none reads a working tree in the middle of a dispatch. A project that is busy
is reported and skipped.

Only the generated summary line under each entry is rewritten. Headings, path
lines, comments, and anything else in PROJECTS.md are left exactly as they
were, and the file is replaced atomically.

Exits non-zero if a registered path is missing or could not be read, or if a
digest failed to generate. A busy project is not a failure.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var only string
			if len(args) == 1 {
				only = args[0]
			}
			report, err := runSync(cmd.Context(), only, dryRun)
			if err != nil {
				return err
			}
			if err := report.write(cmd.OutOrStdout()); err != nil {
				return err
			}
			if report.failed() {
				// The table already named every problem.
				return errSyncProblems
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report which digests are stale without generating any of them")
	return cmd
}

// tildify shortens a path under home to ~/..., which is how the user wrote it
// in PROJECTS.md and how they think about it.
func tildify(path, home string) string {
	if home == "" || path == home {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

func runSync(ctx context.Context, only string, dryRun bool) (*syncReport, error) {
	// sync owns migration (DESIGN.md §18): it is the mutating command the
	// user ran on purpose, and doctor no longer applies anything.
	sess, err := openSession(ctx, true)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	root, cfg, st := sess.Root, sess.Config, sess.Store

	home, err := project.Home()
	if err != nil {
		return nil, err
	}

	registryPath := contextfs.RegistryPath(root)
	reg, err := contextfs.LoadRegistry(ctx, registryPath)
	if err != nil {
		return nil, err
	}
	if !reg.Present {
		return nil, &missingRegistryError{Path: registryPath}
	}

	report := &syncReport{
		registryPath: registryPath,
		contextRoot:  root,
		problems:     reg.Problems(),
		dryRun:       dryRun,
		digestModel:  cfg.Models.Digest.Provider + "/" + cfg.Models.Digest.Model,
	}

	entries := reg.Entries()

	// Every entry's resolved path, computed before any filtering: orphan
	// detection asks what the whole registry points at, so `apex sync <one>`
	// must not report the rest of the portfolio as orphaned.
	registryPaths := map[string]bool{}
	for _, e := range entries {
		if c, err := project.Resolve(e, home); err == nil {
			registryPaths[c.Path] = true
		}
	}

	if only != "" {
		entries = filterEntries(entries, only)
		if len(entries) == 0 {
			return nil, &unknownProjectError{Name: only, Path: registryPath}
		}
	}

	now := time.Now()
	var stale []*project.DigestSource
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, summary, src := syncOne(ctx, st, root, home, entry, now)
		if row.State == stateOK && summary != "" {
			if err := reg.SetSummary(entry.Name, summary); err != nil {
				return nil, fmt.Errorf("regenerate summary for %s: %w", entry.Name, err)
			}
		}
		if row.needsDigest() {
			report.regenerate = append(report.regenerate, entry.Name)
			stale = append(stale, src)
		}
		report.rows = append(report.rows, row)
	}

	orphans, err := project.Orphans(ctx, st, registryPaths)
	if err != nil {
		return nil, err
	}
	report.orphans = orphans

	if reg.Changed() {
		if err := reg.Save(ctx); err != nil {
			return nil, err
		}
		report.rewritten = true
	}

	// The registry is written before any digest is generated, on purpose: it
	// is the cheap, deterministic half of the sync, and a model call that
	// fails halfway must not cost the user the part that already succeeded.
	if dryRun || len(stale) == 0 {
		return report, nil
	}

	adv := sess.advisorFor()
	actx, err := adv.LoadContext(ctx)
	if err != nil {
		return nil, err
	}
	report.missingIdentity = actx.MissingIdentity()

	results, err := adv.RefreshDigests(ctx, actx, stale, cfg.Sync.DigestWorkers)
	if err != nil {
		return nil, err
	}
	report.digests = results
	return report, nil
}

// syncOne processes a single registry entry and never returns an error: every
// failure is a reported, skipped row, because one project must not abort the
// run (DESIGN.md §14, step 2).
func syncOne(ctx context.Context, st *store.Store, root, home string,
	entry contextfs.Entry, now time.Time) (syncRow, string, *project.DigestSource) {

	row := syncRow{Name: entry.Name}

	candidate, err := project.Resolve(entry, home)
	if err != nil {
		row.State = stateError
		row.Detail = err.Error()
		return row, "", nil
	}
	row.Name = candidate.Name
	row.Detail = tildify(candidate.Path, home)

	if err := candidate.Verify(); err != nil {
		row.State = stateMissing
		row.Detail = err.Error()
		return row, "", nil
	}

	// Shared, not exclusive: several syncs may read at once, but none may read
	// a tree that a dispatch is writing (DESIGN.md §10).
	l, err := lock.Acquire(candidate.LockPath(root), lock.Shared)
	if err != nil {
		var busy *lock.BusyError
		if errors.As(err, &busy) {
			row.State = stateBusy
			row.Detail = "in use: " + describeHolder(busy)
			return row, "", nil
		}
		row.State = stateError
		row.Detail = err.Error()
		return row, "", nil
	}

	src, assembleErr := project.AssembleSource(ctx, candidate)
	// Hold the lock no longer than the reads it protects.
	if relErr := l.Release(); relErr != nil && assembleErr == nil {
		assembleErr = relErr
	}
	if assembleErr != nil {
		row.State = stateError
		row.Detail = assembleErr.Error()
		return row, "", nil
	}

	row.Git = src.Git.Describe()

	upserted, err := project.Upsert(ctx, st, candidate, src.Doc.Status(), now)
	if err != nil {
		row.State = stateError
		row.Detail = err.Error()
		return row, "", nil
	}

	freshness, cached, err := project.CheckFreshness(ctx, st, candidate.Slug, src.SourceHash)
	if err != nil {
		row.State = stateError
		row.Detail = err.Error()
		return row, "", nil
	}
	row.Freshness = freshness
	switch freshness {
	case project.Fresh:
		row.Digest = "fresh"
	case project.Stale:
		row.Digest = fmt.Sprintf("stale (%s -> %s)", short(cached), short(src.SourceHash))
	default:
		row.Digest = fmt.Sprintf("none (%s)", short(src.SourceHash))
	}

	var notes []string
	switch {
	case upserted.RenamedFrom != "":
		// The digest and every action item came across with it.
		notes = append(notes, "re-keyed from "+upserted.RenamedFrom)
	case upserted.MovedFrom != "":
		notes = append(notes, "moved from "+tildify(upserted.MovedFrom, home))
	case upserted.Registered:
		notes = append(notes, "newly registered")
	}
	if !src.HasProjectDoc() {
		notes = append(notes, "no "+contextfs.ProjectFile)
	}
	// A note about a directory that is simply not a repository is already the
	// whole of the GIT column; repeating it here would say nothing new.
	if src.Git.Note != "" && src.Git.IsRepo {
		notes = append(notes, src.Git.Note)
	}
	if len(notes) > 0 {
		row.Detail += " (" + strings.Join(notes, "; ") + ")"
	}

	// With no PROJECT.md there is nothing to generate a summary from, and
	// blanking the user's line to say so would be worse than leaving it.
	return row, project.Summarize(src.Doc), src
}

func filterEntries(entries []contextfs.Entry, only string) []contextfs.Entry {
	wantSlug := project.Slug(only)
	var out []contextfs.Entry
	for _, e := range entries {
		if strings.EqualFold(e.Name, only) || project.Slug(e.Name) == wantSlug {
			out = append(out, e)
		}
	}
	return out
}

func describeHolder(busy *lock.BusyError) string {
	h := busy.HolderInfo()
	if h.RunID == "" {
		return "another apex process"
	}
	desc := fmt.Sprintf("run %s (pid %d)", h.RunID, h.PID)
	if !h.Started.IsZero() {
		desc += fmt.Sprintf(", started %s", h.Started.Format(time.RFC3339))
	}
	return desc
}

func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

// errSyncProblems makes `apex sync` exit non-zero without printing anything
// beyond the report it already wrote, in the same style as `apex doctor`.
var errSyncProblems = syncProblemsError{}

type syncProblemsError struct{}

func (syncProblemsError) Error() string     { return "sync found problems" }
func (syncProblemsError) UserFixable() bool { return true }

// missingRegistryError is returned when there is no PROJECTS.md to sync.
type missingRegistryError struct{ Path string }

func (e *missingRegistryError) Error() string {
	return fmt.Sprintf("no project registry at %s\n"+
		"  create it and register a project:\n"+
		"      # Projects\n"+
		"\n"+
		"      ## MyProject\n"+
		"      path: ~/Development/MyProject", e.Path)
}

func (e *missingRegistryError) UserFixable() bool { return true }

// unknownProjectError is returned when `apex sync <name>` names nothing.
type unknownProjectError struct{ Name, Path string }

func (e *unknownProjectError) Error() string {
	return fmt.Sprintf("no project named %q in %s", e.Name, e.Path)
}

func (e *unknownProjectError) UserFixable() bool { return true }

// unsupportedBackendError mirrors what `apex doctor` reports for a backend this
// build cannot open.
type unsupportedBackendError struct{ Backend, ConfigPath string }

func (e *unsupportedBackendError) Error() string {
	return fmt.Sprintf("store backend %q is not implemented in this build\n"+
		"  set store.backend = \"local\" in %s", e.Backend, e.ConfigPath)
}

func (e *unsupportedBackendError) UserFixable() bool { return true }
