package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/RazerBlade6/apex/internal/store"
)

// The projects view (DESIGN.md §12): the registry, with digest previews and
// staleness indicators.
//
// Staleness is reported from `digests.generated_at`, not from
// `projects.last_synced_at`. The two differ in the case that matters: a sync
// that found the source hash unchanged marks the project synced without
// regenerating anything, so last_synced_at moves while the digest the advisor
// actually reasons over does not. The digest's own age is the honest number,
// and a project with no digest row at all is reported as having none rather
// than as being infinitely old.

type projectsState struct {
	projects []store.Project
	digests  map[string]store.Digest
	cursor   int
	offset   int
	height   int
	loaded   bool
	loadErr  error
}

type projectsLoadedMsg struct {
	projects []store.Project
	digests  []store.Digest
	err      error
}

func (m *Model) projectsHint() string {
	if m.projects.loadErr != nil {
		return m.projects.loadErr.Error()
	}
	stale := 0
	missing := 0
	for _, p := range m.projects.projects {
		d, ok := m.projects.digests[p.Slug]
		switch {
		case !ok:
			missing++
		case m.now().Sub(d.GeneratedAt) > staleAfter:
			stale++
		}
	}
	s := fmt.Sprintf("%d project(s)", len(m.projects.projects))
	if missing > 0 {
		s += fmt.Sprintf(" · %d with no digest", missing)
	}
	if stale > 0 {
		s += fmt.Sprintf(" · %d stale", stale)
	}
	if missing > 0 || stale > 0 {
		s += " · run apex sync"
	}
	return s
}

func (m *Model) updateProjects(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case projectsLoadedMsg:
		m.projects.loaded = true
		m.projects.loadErr = msg.err
		if msg.err != nil {
			return m, nil
		}
		m.projects.projects = msg.projects
		m.projects.digests = make(map[string]store.Digest, len(msg.digests))
		for _, d := range msg.digests {
			m.projects.digests[d.ProjectSlug] = d
		}
		if m.projects.cursor >= len(m.projects.projects) {
			m.projects.cursor = 0
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.projects.cursor > 0 {
				m.projects.cursor--
			}
		case "down", "j":
			if m.projects.cursor < len(m.projects.projects)-1 {
				m.projects.cursor++
			}
		case "r":
			return m, m.loadProjectsCmd()
		}
	}
	return m, nil
}

func (m *Model) projectsView() string {
	h := m.projects.height
	switch {
	case m.projects.loadErr != nil:
		return padLines(styleError.Render("could not read the registry: "+m.projects.loadErr.Error()), h)
	case !m.projects.loaded:
		return padLines(styleDim.Render("loading…"), h)
	case len(m.projects.projects) == 0:
		return padLines(styleDim.Render(
			"No projects registered. Add them to ~/.apex/context/PROJECTS.md, then run `apex sync`."), h)
	}

	// The list gets the top half; the selected project's digest preview gets
	// the rest, because a digest is prose and a one-line preview of it says
	// nothing useful.
	listH := h / 2
	if listH < 3 {
		listH = 3
	}
	previewH := h - listH - 1
	if previewH < 3 {
		previewH = 3
	}

	if m.projects.cursor < m.projects.offset {
		m.projects.offset = m.projects.cursor
	}
	if m.projects.cursor >= m.projects.offset+listH {
		m.projects.offset = m.projects.cursor - listH + 1
	}

	var b strings.Builder
	end := min(m.projects.offset+listH, len(m.projects.projects))
	for i := m.projects.offset; i < end; i++ {
		if i > m.projects.offset {
			b.WriteString("\n")
		}
		b.WriteString(m.renderProjectRow(m.projects.projects[i], i == m.projects.cursor))
	}
	return padLines(b.String(), listH) + "\n" + padLines(m.renderDigestPreview(previewH), previewH)
}

func (m *Model) renderProjectRow(p store.Project, selected bool) string {
	marker := "  "
	if selected {
		marker = "› "
	}
	d, ok := m.projects.digests[p.Slug]
	age := "no digest"
	if ok {
		age = digestAge(m.now(), d.GeneratedAt)
	}
	// Padded out to the full inner width, so the selected row's background
	// reaches the edge of the box instead of stopping at the path.
	w := m.innerWidth()
	line := padTo(truncate(fmt.Sprintf("%s%-22s %-12s %s", marker, p.Name, age, p.Path), w), w)
	switch {
	case selected:
		return styleSelected.Render(line)
	case !ok:
		return styleWarn.Render(line)
	case m.now().Sub(d.GeneratedAt) > staleAfter:
		return styleWarn.Render(line)
	default:
		return line
	}
}

func (m *Model) renderDigestPreview(height int) string {
	if m.projects.cursor >= len(m.projects.projects) {
		return ""
	}
	p := m.projects.projects[m.projects.cursor]
	d, ok := m.projects.digests[p.Slug]
	if !ok {
		return styleDim.Render(p.Name + " has no digest yet. Run `apex sync` to generate one.")
	}

	head := styleLabel.Render(p.Name) + styleDim.Render(fmt.Sprintf("  %s · %s",
		digestAge(m.now(), d.GeneratedAt), orDash(d.Model)))
	body := wrapPlain(d.Body, m.innerWidth())
	lines := strings.Split(body, "\n")
	if len(lines) > height-1 {
		lines = lines[:max(height-2, 1)]
		lines = append(lines, styleDim.Render("…"))
	}
	return head + "\n" + strings.Join(lines, "\n")
}

// digestAge renders how long ago a digest was generated.
func digestAge(now, at time.Time) string {
	d := now.Sub(at)
	switch {
	case d < time.Hour:
		return "just now"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < staleAfter:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dd ago ·stale", int(d.Hours()/24))
	}
}
