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
	"github.com/RazerBlade6/apex/internal/provider"
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

By default nothing here talks to a model API: a key that exists is reported as
present, which is not the same as reported as working. --probe makes one
minimal, deliberately cheap request per configured provider to find out which
it is. That request costs money, so it is opt-in.`,
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
		"make one minimal API call per configured provider to verify the key actually works")
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
	// resolves the key from the keychain or the environment and talks to the
	// vendor.
	newProvider func(ctx context.Context, route config.ModelRoute) (provider.Provider, error)
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
		if opts.probe {
			checkProbe(ctx, r, cfg, opts)
		}
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
			return registry.New(ctx, route, registry.WithMaxRetries(0))
		}
	}

	for _, route := range probeRoutes(cfg) {
		name := route.Provider + " probe"

		p, err := newProvider(ctx, route)
		if err != nil {
			var missing *config.MissingKeyError
			if errors.As(err, &missing) {
				r.fail(name, fmt.Sprintf("no key to probe with: not in %s or $%s",
					missing.Service, missing.EnvVar),
					fmt.Sprintf("security add-generic-password -s %s -a %s -w\nor: export %s=...\nthen rerun apex doctor --probe",
						missing.Service, config.KeychainUser, missing.EnvVar))
				continue
			}
			r.fail(name, err.Error(), fmt.Sprintf("check models.*.provider in %s", cfg.Path()))
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

// reportProbeFailure turns a classified provider error into the line the user
// acts on.
func reportProbeFailure(r *report, name string, route config.ModelRoute, cfg *config.Config, err error) {
	service, envVar, locErr := config.CredentialLocation(route.Provider)
	if locErr != nil {
		service, envVar = "the keychain", "the environment"
	}

	var perr *provider.Error
	if !errors.As(err, &perr) {
		r.fail(name, err.Error(), "")
		return
	}

	switch perr.Kind {
	case provider.KindAuth:
		r.fail(name, fmt.Sprintf("key rejected (HTTP %d): %s", perr.StatusCode, perr.Message),
			fmt.Sprintf("the key resolved for %s is present but not accepted\nreplace it: security add-generic-password -U -s %s -a %s -w\nor correct $%s",
				route.Provider, service, config.KeychainUser, envVar))
	case provider.KindRateLimit:
		r.warn(name, fmt.Sprintf("rate limited (HTTP %d): the key is valid, the account is over its limit", perr.StatusCode),
			"wait and rerun; --probe deliberately does not retry, so this is the provider's answer and not a timeout")
	case provider.KindNetwork:
		r.fail(name, "network unreachable: no HTTP response arrived: "+perr.Message,
			"check connectivity and any proxy; the key was never sent anywhere")
	case provider.KindCancelled:
		r.fail(name, fmt.Sprintf("no answer within %s", probeTimeout),
			"rerun when the provider is responding; nothing here proves the key is wrong")
	case provider.KindRequest:
		r.fail(name, fmt.Sprintf("request rejected (HTTP %d): %s", perr.StatusCode, perr.Message),
			fmt.Sprintf("the key was accepted and the request was not\ncheck models.*.model in %s: %q must be an exact model id",
				cfg.Path(), route.Model))
	case provider.KindServer:
		r.warn(name, fmt.Sprintf("provider error (HTTP %d): %s", perr.StatusCode, perr.Message),
			"this is the vendor's failure, not yours; rerun later")
	default:
		r.fail(name, perr.Error(), "")
	}
}
