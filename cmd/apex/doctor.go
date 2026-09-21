package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/store"
)

// level is how bad a check result is. Exit status is driven by fail alone:
// a warn is something to know about, not something that blocks work.
type level int

const (
	levelOK level = iota
	levelWarn
	levelFail
)

func (l level) String() string {
	switch l {
	case levelOK:
		return "ok"
	case levelWarn:
		return "warn"
	default:
		return "FAIL"
	}
}

// check is one line of the doctor report. Fix is printed below the table and
// must say what to actually do, not merely restate the problem.
type check struct {
	Name   string
	Level  level
	Detail string
	Fix    string
}

// report accumulates checks in the order they ran.
type report struct {
	checks []check
}

func (r *report) add(c check) { r.checks = append(r.checks, c) }

func (r *report) ok(name, detail string) {
	r.add(check{Name: name, Level: levelOK, Detail: detail})
}

func (r *report) warn(name, detail, fix string) {
	r.add(check{Name: name, Level: levelWarn, Detail: detail, Fix: fix})
}

func (r *report) fail(name, detail, fix string) {
	r.add(check{Name: name, Level: levelFail, Detail: detail, Fix: fix})
}

func (r *report) failed() bool {
	for _, c := range r.checks {
		if c.Level == levelFail {
			return true
		}
	}
	return false
}

func (r *report) write(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tCHECK\tDETAIL")
	for _, c := range r.checks {
		// A multi-line detail would break the table; fold it.
		detail := strings.ReplaceAll(strings.TrimSpace(c.Detail), "\n", "; ")
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Level, c.Name, detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	var problems []check
	for _, c := range r.checks {
		if c.Level != levelOK && c.Fix != "" {
			problems = append(problems, c)
		}
	}
	if len(problems) == 0 {
		fmt.Fprintln(w, "\nEverything checks out.")
		return nil
	}
	fmt.Fprintln(w, "\nWhat to do:")
	for _, c := range problems {
		fmt.Fprintf(w, "  %s (%s)\n", c.Name, c.Level)
		for _, line := range strings.Split(strings.TrimRight(c.Fix, "\n"), "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	return nil
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Verify toolchain, credentials, storage, and executor availability",
		Long: `Check every environment assumption Apex makes, and report exactly what is
missing and how to fix it. Exits non-zero if anything required is broken.

Silently assuming the environment is the failure mode this project most wants
to avoid, so doctor is specific rather than reassuring.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r := runDoctor(cmd.Context())
			if err := r.write(cmd.OutOrStdout()); err != nil {
				return err
			}
			if r.failed() {
				// The table already said what is wrong; a second error
				// message would only repeat it.
				return errSilentFail
			}
			return nil
		},
	}
}

// errSilentFail makes `apex doctor` exit non-zero without printing anything
// beyond the report it already wrote.
var errSilentFail = silentError{}

type silentError struct{}

func (silentError) Error() string     { return "doctor found problems" }
func (silentError) UserFixable() bool { return true }

func runDoctor(ctx context.Context) *report {
	r := &report{}

	checkGoToolchain(ctx, r)
	checkBinary(ctx, r, binarySpec{
		name:     "git",
		bin:      "git",
		args:     []string{"--version"},
		required: true,
		fix:      "install the Xcode command line tools: xcode-select --install",
	})
	checkBinary(ctx, r, binarySpec{
		name:     "claude (executor)",
		bin:      "claude",
		args:     []string{"--version"},
		required: true,
		extraDirs: []string{
			filepath.Join(os.Getenv("HOME"), ".local", "bin"),
		},
		fix: "install the Claude Code CLI and ensure it is on PATH\n" +
			"apex dispatches implementation work to it (DESIGN.md §9)",
	})

	checkLayout(ctx, r)
	cfg := checkConfig(ctx, r)
	if cfg != nil {
		checkKeys(ctx, r, cfg)
		checkStore(ctx, r, cfg)
	}
	return r
}

func checkGoToolchain(ctx context.Context, r *report) {
	// The running binary's toolchain is known for certain; the installed `go`
	// is only needed to rebuild, so its absence is a warning.
	r.ok("go runtime", fmt.Sprintf("%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH))

	path, err := exec.LookPath("go")
	if err != nil {
		r.warn("go toolchain", "go not found on PATH",
			"install Go 1.27+ to build apex from source; a prebuilt binary runs without it")
		return
	}
	out, err := runVersion(ctx, path, "version")
	if err != nil {
		r.warn("go toolchain", fmt.Sprintf("%s: %v", path, err),
			"check that the go on PATH is a working installation")
		return
	}
	r.ok("go toolchain", fmt.Sprintf("%s (%s)", out, path))
}

type binarySpec struct {
	name      string
	bin       string
	args      []string
	required  bool
	extraDirs []string // searched when the binary is not on PATH
	fix       string
}

func checkBinary(ctx context.Context, r *report, spec binarySpec) {
	path, err := exec.LookPath(spec.bin)
	if err != nil {
		for _, dir := range spec.extraDirs {
			candidate := filepath.Join(dir, spec.bin)
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
				path = candidate
				err = nil
				break
			}
		}
	}
	if err != nil || path == "" {
		detail := fmt.Sprintf("%s not found on PATH", spec.bin)
		if len(spec.extraDirs) > 0 {
			detail += " or in " + strings.Join(spec.extraDirs, ", ")
		}
		if spec.required {
			r.fail(spec.name, detail, spec.fix)
		} else {
			r.warn(spec.name, detail, spec.fix)
		}
		return
	}

	version, err := runVersion(ctx, path, spec.args...)
	if err != nil {
		r.fail(spec.name, fmt.Sprintf("%s is present but did not run: %v", path, err),
			"check the binary is executable and not quarantined")
		return
	}
	r.ok(spec.name, fmt.Sprintf("%s (%s)", version, path))
}

// runVersion executes a binary's version command and returns its first line.
func runVersion(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, args...).Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}

func checkLayout(ctx context.Context, r *report) {
	root, err := config.Root()
	if err != nil {
		r.fail("state directory", err.Error(), "set APEX_HOME to a writable directory")
		return
	}
	created, err := config.EnsureLayout(ctx)
	if err != nil {
		r.fail("state directory", fmt.Sprintf("%s: %v", root, err),
			fmt.Sprintf("ensure %s is writable", root))
		return
	}
	detail := root
	if len(created) > 0 {
		rel := make([]string, 0, len(created))
		for _, dir := range created {
			if p, relErr := filepath.Rel(root, dir); relErr == nil && p != "." {
				rel = append(rel, p)
			} else {
				rel = append(rel, dir)
			}
		}
		detail += " (created " + strings.Join(rel, ", ") + ")"
	}
	r.ok("state directory", detail)
}

func checkConfig(ctx context.Context, r *report) *config.Config {
	cfg, err := config.Load(ctx)
	if err != nil {
		path, _ := config.FilePath()
		r.fail("config", err.Error(), fmt.Sprintf("fix or delete %s and rerun; apex rewrites the defaults", path))
		return nil
	}

	problems := cfg.Validate()
	if len(problems) > 0 {
		msgs := make([]string, 0, len(problems))
		for _, p := range problems {
			msgs = append(msgs, p.Error())
		}
		sort.Strings(msgs)
		r.fail("config", fmt.Sprintf("%s: %s", cfg.Path(), strings.Join(msgs, "; ")),
			fmt.Sprintf("edit %s\nsee DESIGN.md §8 and §13 for the accepted values", cfg.Path()))
		return cfg
	}

	r.ok("config", fmt.Sprintf("%s (advisor=%s/%s, chat=%s/%s, digest=%s/%s)",
		cfg.Path(),
		cfg.Models.Advisor.Provider, cfg.Models.Advisor.Model,
		cfg.Models.Chat.Provider, cfg.Models.Chat.Model,
		cfg.Models.Digest.Provider, cfg.Models.Digest.Model))
	return cfg
}

func checkKeys(ctx context.Context, r *report, cfg *config.Config) {
	inUse := map[string]bool{}
	for _, p := range cfg.Providers() {
		inUse[p] = true
	}

	for _, provider := range config.ValidProviders {
		name := provider + " key"
		key, err := config.ResolveKey(ctx, provider)
		if err == nil {
			// Never the key itself: only where it came from.
			detail := fmt.Sprintf("resolved from %s (%s)", key.Source, key.Origin)
			if !inUse[provider] {
				detail += "; not referenced by config"
			}
			r.ok(name, detail)
			continue
		}

		var missing *config.MissingKeyError
		if !errors.As(err, &missing) {
			r.fail(name, err.Error(), "")
			continue
		}
		detail := fmt.Sprintf("not in %s or $%s", missing.Service, missing.EnvVar)
		fix := fmt.Sprintf("security add-generic-password -s %s -a %s -w\nor: export %s=...",
			missing.Service, config.KeychainUser, missing.EnvVar)
		if inUse[provider] {
			r.fail(name, detail+" (required: config routes work to it)", fix)
		} else {
			r.warn(name, detail+" (not referenced by config)", fix)
		}
	}
}

func checkStore(ctx context.Context, r *report, cfg *config.Config) {
	if cfg.Store.Backend != "local" {
		r.fail("store backend",
			fmt.Sprintf("backend %q is not implemented in this build", cfg.Store.Backend),
			fmt.Sprintf("set store.backend = \"local\" in %s\nthe turso backend arrives with remote persistence", cfg.Path()))
		return
	}

	dbPath, err := config.DatabasePath()
	if err != nil {
		r.fail("store backend", err.Error(), "set APEX_HOME to a writable directory")
		return
	}
	backend := store.NewLocalBackend(dbPath)
	r.ok("store backend", backend.Describe())

	st, err := store.Open(ctx, backend)
	if err != nil {
		r.fail("store", err.Error(), fmt.Sprintf("ensure %s is writable", dbPath))
		return
	}
	defer backend.Close()

	if !st.Online() {
		r.fail("store", "backend reports writes cannot currently succeed",
			"reconnect, or switch store.backend to \"local\"")
		return
	}

	if err := st.Migrate(ctx); err != nil {
		fix := "rerun once the other migrator finishes"
		if errors.Is(err, store.ErrOfflineMigration) {
			fix = "reconnect: applying a migration requires writing to the primary"
		}
		r.fail("migrations", err.Error(), fix)
		return
	}

	status, err := st.Status(ctx)
	if err != nil {
		r.fail("migrations", err.Error(), "")
		return
	}
	if len(status.Pending) > 0 {
		names := make([]string, 0, len(status.Pending))
		for _, m := range status.Pending {
			names = append(names, fmt.Sprintf("%03d_%s", m.Version, m.Name))
		}
		r.fail("migrations", "pending after migrating: "+strings.Join(names, ", "), "rerun apex doctor")
		return
	}
	detail := fmt.Sprintf("%d applied, 0 pending", len(status.Applied))
	if n := len(status.Applied); n > 0 {
		last := status.Applied[n-1]
		detail += fmt.Sprintf(" (latest %03d_%s, %s)",
			last.Version, last.Name, last.AppliedAt.Format(time.RFC3339))
	}
	r.ok("migrations", detail)
}
