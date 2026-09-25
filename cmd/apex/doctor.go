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
	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/executor"
	"github.com/RazerBlade6/apex/internal/executor/claudecode"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/provider/claudecli"
	"github.com/RazerBlade6/apex/internal/provider/registry"
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
	var opts doctorOptions

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify toolchain, credentials, storage, and executor availability",
		Long: `Check every environment assumption Apex makes, and report exactly what is
missing and how to fix it. Exits non-zero if anything required is broken.

Silently assuming the environment is the failure mode this project most wants
to avoid, so doctor is specific rather than reassuring.

By default nothing here talks to a model API: a credential that exists is
reported as present, which is not the same as reported as working. --probe
makes one minimal, deliberately cheap request per configured provider to find
out which it is. That request costs money, so it is opt-in.

For a claude-cli route the cost is not money but session quota, which is
shared with your own Claude Code usage, so --probe spends a little of what you
may want for real work.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r := runDoctor(cmd.Context(), opts)
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

	cmd.Flags().BoolVar(&opts.probe, "probe", false,
		"make one minimal call per configured provider to verify the credential actually works")
	return cmd
}

// errSilentFail makes `apex doctor` exit non-zero without printing anything
// beyond the report it already wrote.
var errSilentFail = silentError{}

type silentError struct{}

func (silentError) Error() string     { return "doctor found problems" }
func (silentError) UserFixable() bool { return true }

// doctorOptions carries the flags, plus the one seam the probe needs to be
// testable without a credential or a network.
type doctorOptions struct {
	// probe makes one minimal API call per configured provider.
	probe bool

	// newProvider overrides how a provider is constructed for --probe. Tests
	// point it at an httptest server; nil means the real registry, which
	// resolves the credential and talks to the vendor.
	newProvider func(ctx context.Context, route config.ModelRoute) (provider.Provider, error)

	// claudeBin overrides the claude binary the subscription checks run.
	// Tests point it at a fake; empty means the real one, looked up on PATH
	// and then in the directories its installer uses.
	claudeBin string
}

func runDoctor(ctx context.Context, opts doctorOptions) *report {
	r := &report{}

	checkGoToolchain(ctx, r)
	checkBinary(ctx, r, binarySpec{
		name:     "git",
		bin:      "git",
		args:     []string{"--version"},
		required: true,
		fix:      "install the Xcode command line tools: xcode-select --install",
	})
	checkExecutor(ctx, r, opts)

	checkLayout(ctx, r)
	checkIdentity(ctx, r)
	cfg := checkConfig(ctx, r)
	if cfg != nil {
		checkKeys(ctx, r, cfg)
		checkClaudeAuth(ctx, r, cfg, opts)
		checkDigestRoute(ctx, r, cfg)
		checkStore(ctx, r, cfg)
		if opts.probe {
			checkProbe(ctx, r, cfg, opts)
		}
	}
	return r
}

// checkExecutor asks the executor itself whether it can run, then confirms
// the binary it resolved actually executes.
//
// DESIGN.md §18 recorded the reason this is not a hand-rolled lookup any
// more: "doctor resolves the claude binary twice, two different ways. The M1
// executor check has its own lookup; claudecli.LookPath is a second. They
// agree today. M5 should collapse the executor onto the provider's lookup so
// they cannot drift." Both now go through internal/claudecmd, and this check
// reaches it through Executor.Available() — so what doctor reports is
// literally the same question a dispatch asks, rather than a second
// implementation of it that happens to agree.
func checkExecutor(ctx context.Context, r *report, opts doctorOptions) {
	name := "executor (" + claudecode.Name + ")"
	exec := claudecode.New(claudecode.Options{Binary: opts.claudeBin})

	if err := exec.Available(); err != nil {
		var unavailable *executor.UnavailableError
		fix := "install the Claude Code CLI and ensure it is on PATH"
		if errors.As(err, &unavailable) && unavailable.Fix != "" {
			fix = unavailable.Fix
		}
		r.fail(name, err.Error(), fix)
		return
	}

	// Available() resolves the binary and deliberately stops there, because a
	// dispatch should not pay for a process start to ask a question it is
	// about to answer anyway. doctor is where the slower, more thorough
	// answer belongs: a quarantined or non-executable binary resolves fine
	// and still cannot run.
	bin, err := exec.Binary()
	if err != nil {
		r.fail(name, err.Error(), "install the Claude Code CLI and ensure it is on PATH")
		return
	}
	version, err := runVersion(ctx, bin, "--version")
	if err != nil {
		r.fail(name, fmt.Sprintf("%s is present but did not run: %v", bin, err),
			"check the binary is executable and not quarantined")
		return
	}
	r.ok(name, fmt.Sprintf("%s (%s)", version, bin))
}

// checkIdentity reports whether the identity documents exist (DESIGN.md §6).
//
// This closes the third M5 item in §18. PROFILE.md and SKILLS.md are loaded
// into the system prompt of every advisor call, and when they are absent the
// loop still runs — it just reasons from project digests alone and produces
// generic advice about code rather than advice for this user. That failure is
// completely silent from the outside: the output looks like output. doctor
// exists to catch exactly the assumptions that fail quietly, so it says so.
//
// A missing file is a warning, not a failure. Apex works without them, and
// the advisor commands already say the same thing at the point of use; what
// doctor adds is that you find out before you have judged the output.
func checkIdentity(ctx context.Context, r *report) {
	const name = "identity context"

	root, err := config.Root()
	if err != nil {
		r.fail(name, err.Error(), "set APEX_HOME to a writable directory")
		return
	}
	identity, err := contextfs.LoadIdentity(ctx, root)
	if err != nil {
		r.fail(name, err.Error(), fmt.Sprintf("ensure %s is readable", contextfs.ContextDir(root)))
		return
	}

	var present, missing, empty []string
	for _, doc := range []struct {
		file string
		doc  contextfs.Document
	}{
		{contextfs.ProfileFile, identity.Profile},
		{contextfs.SkillsFile, identity.Skills},
	} {
		switch {
		case !doc.doc.Present:
			missing = append(missing, doc.file)
		case doc.doc.Empty():
			empty = append(empty, doc.file)
		default:
			present = append(present, fmt.Sprintf("%s (%d bytes)", doc.file, len(doc.doc.Body)))
		}
	}

	if len(missing) == 0 && len(empty) == 0 {
		r.ok(name, strings.Join(present, ", ")+" in "+contextfs.ContextDir(root))
		return
	}

	var detail []string
	if len(present) > 0 {
		detail = append(detail, strings.Join(present, ", "))
	}
	if len(missing) > 0 {
		detail = append(detail, strings.Join(missing, " and ")+" absent")
	}
	if len(empty) > 0 {
		detail = append(detail, strings.Join(empty, " and ")+" present but empty")
	}
	r.warn(name, strings.Join(detail, "; "),
		fmt.Sprintf("write %s in %s\n"+
			"apex loads both into every advisor call; without them review and ideas reason from\n"+
			"project digests alone and produce generic advice about the code rather than advice for you",
			strings.Join(append(missing, empty...), " and "), contextfs.ContextDir(root)))
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

	// Only the providers that actually have a key. A claude-cli route has
	// none, and reporting it here as "not in the keychain" would send the
	// user to add a key nothing would ever read (DESIGN.md §13). Its
	// credential is checked by checkClaudeAuth instead.
	for _, provider := range config.KeyProviders() {
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

// usesClaudeCLI reports whether any model route is subscription-backed.
func usesClaudeCLI(cfg *config.Config) bool {
	for _, name := range cfg.Providers() {
		if name == claudecli.Name {
			return true
		}
	}
	return false
}

// checkClaudeAuth verifies the credential behind a claude-cli route
// (DESIGN.md §13).
//
// It runs only when a route actually uses it, because a user on API keys
// should not be told about a CLI login they do not need. Three states are
// kept apart, since they have three different fixes: the CLI is not
// installed, it is installed but logged out, or it is logged in. A fourth is
// reported as a warning — logged in, but with an API key rather than the
// subscription, which means a route chosen to spend subscription quota is
// quietly spending money instead.
//
// Nothing here reads the credential. `claude auth status` reports that one
// exists and which account it belongs to; the OAuth token itself never enters
// Apex's process.
func checkClaudeAuth(ctx context.Context, r *report, cfg *config.Config, opts doctorOptions) {
	if !usesClaudeCLI(cfg) {
		return
	}
	const name = "claude auth"

	status, err := claudecli.Auth(ctx, opts.claudeBin)
	if err != nil {
		var perr *provider.Error
		if errors.As(err, &perr) && perr.Kind == provider.KindUnavailable {
			r.fail(name, perr.Message,
				"install the Claude Code CLI and ensure it is on PATH\n"+
					fmt.Sprintf("a route in %s is set to provider = %q, which runs inference through it", cfg.Path(), claudecli.Name))
			return
		}
		r.fail(name, err.Error(), "check that `claude auth status --json` runs and prints JSON")
		return
	}

	switch {
	case !status.LoggedIn:
		r.fail(name, "the claude CLI is installed but not logged in",
			"run: claude auth login\n"+
				"apex never reads or stores that credential; it only checks that the CLI has one")
	case !status.Subscription():
		r.warn(name, status.Describe()+"; this is not a claude.ai subscription login",
			fmt.Sprintf("a claude-cli route exists to spend subscription quota, and this login would bill an API account instead\nrun: claude auth login\nor set the route back to provider = \"anthropic\" in %s", cfg.Path()))
	default:
		r.ok(name, status.Describe())
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

	// doctor is READ-ONLY (DESIGN.md §18). It used to call st.Migrate here,
	// which meant that during M3 a plain `apex doctor` applied migration 002
	// to the real database with no prompt and no mention of having done so —
	// a command whose entire job is verifying the environment performing a
	// one-way, forward-only schema change. Pending migrations are reported;
	// `apex sync` applies them.
	status, err := st.Status(ctx)
	if err != nil {
		r.fail("migrations", err.Error(), "")
		return
	}
	detail := fmt.Sprintf("%d applied", len(status.Applied))
	if n := len(status.Applied); n > 0 {
		last := status.Applied[n-1]
		detail += fmt.Sprintf(" (latest %03d_%s, %s)",
			last.Version, last.Name, last.AppliedAt.Format(time.RFC3339))
	}

	if len(status.Pending) > 0 {
		names := make([]string, 0, len(status.Pending))
		for _, m := range status.Pending {
			names = append(names, fmt.Sprintf("%03d_%s", m.Version, m.Name))
		}
		// A warning, not a failure: the schema being behind is a normal
		// state after an upgrade, and it is one command away from fixed.
		r.warn("migrations",
			fmt.Sprintf("%s, %d pending: %s", detail, len(status.Pending), strings.Join(names, ", ")),
			"run: apex sync\n"+
				"doctor does not apply migrations: they are forward-only and cannot be rolled back,\n"+
				"so a command that only verifies the environment must not make one")
		return
	}
	r.ok("migrations", detail+", 0 pending")
}

// checkDigestRoute warns about spending subscription quota on bulk work when
// an API key is sitting right there (DESIGN.md §8, "the binding constraint is
// quota contention").
//
// The condition is narrower than §18 originally proposed, and deliberately
// so. §18 asked for a warn on models.digest.provider == "claude-cli" full
// stop. But the digest slot is the high-volume one, and the reason to move it
// off the subscription is that the session window is shared with the user's
// own Claude Code work — which is only advice worth giving if the user has
// somewhere else to put it. On an all-subscription machine with no API key at
// all, that warning is a recurring complaint about a choice with no
// alternative, and a doctor that nags about the only possible configuration
// teaches the user to stop reading its warnings.
//
// So it fires on the combination that is actually a mistake: digests routed
// at the subscription while a key for a key-based provider resolves. §18 has
// been updated to match.
func checkDigestRoute(ctx context.Context, r *report, cfg *config.Config) {
	if cfg.Models.Digest.Provider != claudecli.Name {
		return
	}
	const name = "digest route"

	var available []string
	for _, p := range config.KeyProviders() {
		if _, err := config.ResolveKey(ctx, p); err == nil {
			available = append(available, p)
		}
	}
	if len(available) == 0 {
		return
	}
	r.warn(name,
		fmt.Sprintf("bulk digest generation is routed at %s while a key for %s is available",
			claudecli.Name, strings.Join(available, " and ")),
		fmt.Sprintf("the subscription window is shared with your own Claude Code sessions, and a full sync is the\n"+
			"easiest way to spend it; the advisor and chat slots are low-volume and belong there instead\n"+
			"in %s:\n  [models.digest]\n  provider = %q\n  model    = \"claude-opus-5\"",
			cfg.Path(), available[0]))
}

// probeMaxTokens keeps the probe request as small as a request can be while
// still exercising the whole path: authentication, the model id, and a
// streamed response. A handful of tokens is enough to prove all three.
const probeMaxTokens = 16

// probeTimeout bounds one probe. A provider that has not answered in this long
// is a finding in itself, and doctor must not hang.
const probeTimeout = 30 * time.Second

// probeRoutes returns one model route per distinct provider, preferring the
// route that matters most. DESIGN.md §8 allows three slots to name three
// different vendors; the probe makes one call per vendor, not per slot.
func probeRoutes(cfg *config.Config) []config.ModelRoute {
	seen := map[string]bool{}
	var out []config.ModelRoute
	for _, route := range []config.ModelRoute{cfg.Models.Advisor, cfg.Models.Chat, cfg.Models.Digest} {
		if route.Provider == "" || seen[route.Provider] {
			continue
		}
		seen[route.Provider] = true
		out = append(out, route)
	}
	return out
}

// checkProbe makes one minimal API call per configured provider.
//
// This is what the user runs immediately after adding a key, so the failure
// modes are kept apart rather than collapsed into "the provider did not
// work": a key that is absent, a key that is rejected, a key that is fine
// behind a rate limit, and a network that never carried the request are four
// different problems with four different fixes.
func checkProbe(ctx context.Context, r *report, cfg *config.Config, opts doctorOptions) {
	newProvider := opts.newProvider
	if newProvider == nil {
		newProvider = func(ctx context.Context, route config.ModelRoute) (provider.Provider, error) {
			// No retries: a 429 is a finding to report, not something to sit
			// through the SDK's backoff for.
			buildOpts := []registry.Option{registry.WithMaxRetries(0)}
			if opts.claudeBin != "" {
				buildOpts = append(buildOpts, registry.WithBinary(opts.claudeBin))
			}
			return registry.New(ctx, route, buildOpts...)
		}
	}

	for _, route := range probeRoutes(cfg) {
		name := route.Provider + " probe"

		p, err := newProvider(ctx, route)
		if err != nil {
			reportProbeSetupFailure(r, name, cfg, err)
			continue
		}

		usage, err := probeOnce(ctx, p, route)
		if err == nil {
			detail := fmt.Sprintf("%s answered (%d input, %d output tokens)",
				route.Model, usage.InputTokens, usage.OutputTokens)
			r.ok(name, detail)
			continue
		}
		reportProbeFailure(r, name, route, cfg, err)
	}
}

// probeOnce sends the smallest useful request and drains the stream.
//
// Effort is forced to low regardless of what the route configures: the probe
// answers "does this key work", which the cheapest possible call answers just
// as well as an expensive one.
func probeOnce(ctx context.Context, p provider.Provider, route config.ModelRoute) (provider.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	events, err := p.Stream(ctx, provider.Request{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "ping"}},
		Model:     route.Model,
		Effort:    "low",
		MaxTokens: probeMaxTokens,
	})
	if err != nil {
		return provider.Usage{}, err
	}

	var usage provider.Usage
	for event := range events {
		switch event.Type {
		case provider.EventError:
			// Keep draining: the contract is that the channel closes, and
			// abandoning it here would strand the sending goroutine until the
			// deferred cancel ran.
			if err == nil {
				err = event.Err
			}
		case provider.EventDone:
			if event.Usage != nil {
				usage = *event.Usage
			}
		}
	}
	return usage, err
}

// reportProbeSetupFailure explains a provider that could not even be built.
//
// The three reasons are different problems with different fixes: no API key
// for a key-based route, no claude binary for a subscription route, and a
// provider name this build does not implement.
func reportProbeSetupFailure(r *report, name string, cfg *config.Config, err error) {
	var missing *config.MissingKeyError
	if errors.As(err, &missing) {
		r.fail(name, fmt.Sprintf("no key to probe with: not in %s or $%s",
			missing.Service, missing.EnvVar),
			fmt.Sprintf("security add-generic-password -s %s -a %s -w\nor: export %s=...\nthen rerun apex doctor --probe",
				missing.Service, config.KeychainUser, missing.EnvVar))
		return
	}
	var perr *provider.Error
	if errors.As(err, &perr) && perr.Kind == provider.KindUnavailable {
		r.fail(name, "nothing to probe with: "+perr.Message,
			"install the Claude Code CLI and ensure it is on PATH")
		return
	}
	r.fail(name, err.Error(), fmt.Sprintf("check models.*.provider in %s", cfg.Path()))
}

// reportProbeFailure turns a classified provider error into the line the user
// acts on.
func reportProbeFailure(r *report, name string, route config.ModelRoute, cfg *config.Config, err error) {
	service, envVar, locErr := config.CredentialLocation(route.Provider)

	// A subscription route has no keychain entry and no environment
	// variable, so every message below has to be phrased for the credential
	// it actually has. Telling the user to add an API key would send them to
	// fix the wrong thing (DESIGN.md §13).
	var keyless *config.KeylessProviderError
	subscription := errors.As(locErr, &keyless)
	if locErr != nil && !subscription {
		service, envVar = "the keychain", "the environment"
	}

	var perr *provider.Error
	if !errors.As(err, &perr) {
		r.fail(name, err.Error(), "")
		return
	}

	switch perr.Kind {
	case provider.KindAuth:
		if subscription {
			r.fail(name, "the claude CLI rejected the request for lack of a credential: "+perr.Message,
				"run: claude auth login\nthen rerun apex doctor --probe")
			return
		}
		r.fail(name, fmt.Sprintf("key rejected (HTTP %d): %s", perr.StatusCode, perr.Message),
			fmt.Sprintf("the key resolved for %s is present but not accepted\nreplace it: security add-generic-password -U -s %s -a %s -w\nor correct $%s",
				route.Provider, service, config.KeychainUser, envVar))
	case provider.KindUnavailable:
		r.fail(name, perr.Message,
			"install the Claude Code CLI and ensure it is on PATH")
	case provider.KindSessionLimit:
		// Deliberately not the rate-limit line. A session limit is not a
		// per-minute API cap that clears in seconds: the window is shared
		// with the user's own Claude Code sessions, and the honest advice is
		// about where to spend the quota, not about waiting a moment.
		detail := "subscription session limit reached: " + perr.Message
		if !perr.ResetsAt.IsZero() {
			detail = fmt.Sprintf("subscription session limit reached, resets %s: %s",
				perr.ResetsAt.Format(time.RFC3339), perr.Message)
		}
		r.fail(name, detail,
			"this is NOT an API rate limit; the credential works and the subscription window is spent\n"+
				"the same window serves your own Claude Code sessions, so apex has been using quota you may want back\n"+
				fmt.Sprintf("wait for the reset, or route the high-volume slots to an API key in %s:\n  [models.digest] provider = \"anthropic\"", cfg.Path()))
	case provider.KindRateLimit:
		r.warn(name, fmt.Sprintf("rate limited (HTTP %d): the key is valid, the account is over its limit", perr.StatusCode),
			"wait and rerun; --probe deliberately does not retry, so this is the provider's answer and not a timeout")
	case provider.KindNetwork:
		r.fail(name, "network unreachable: no HTTP response arrived: "+perr.Message,
			"check connectivity and any proxy; the key was never sent anywhere")
	case provider.KindCancelled:
		r.fail(name, fmt.Sprintf("no answer within %s", probeTimeout),
			"rerun when the provider is responding; nothing here proves the credential is wrong")
	case provider.KindRequest:
		r.fail(name, fmt.Sprintf("request rejected (HTTP %d): %s", perr.StatusCode, perr.Message),
			fmt.Sprintf("the credential was accepted and the request was not\ncheck models.*.model in %s: %q must be a model id this provider accepts",
				cfg.Path(), route.Model))
	case provider.KindServer:
		r.warn(name, fmt.Sprintf("provider error (HTTP %d): %s", perr.StatusCode, perr.Message),
			"this is the vendor's failure, not yours; rerun later")
	default:
		r.fail(name, perr.Error(), "")
	}
}
