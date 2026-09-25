package store

import "time"

// Project is a row of projects (DESIGN.md §7).
type Project struct {
	Slug         string
	Name         string
	Path         string
	Status       string
	RegisteredAt time.Time
	LastSyncedAt *time.Time
}

// Digest is a cached synthesis of one project's current state.
type Digest struct {
	ProjectSlug string
	Body        string
	SourceHash  string
	GeneratedAt time.Time
	Model       string
}

// Action item statuses.
const (
	ItemProposed   = "proposed"
	ItemAccepted   = "accepted"
	ItemInProgress = "in_progress"
	ItemInReview   = "in_review"
	ItemDone       = "done"
	ItemDismissed  = "dismissed"
)

// ActionItem is a row of action_items. IDs are globally sequential (AI-014).
type ActionItem struct {
	ID          string
	ProjectSlug string
	Title       string
	Body        string
	Rationale   string
	Effort      string // small | medium | large
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	GeneratedBy string
	// ContextHash records the digest set that produced this item, so Apex
	// can say an item was generated against a state that has since moved on.
	ContextHash string
}

// Idea statuses.
const (
	IdeaProposed  = "proposed"
	IdeaSaved     = "saved"
	IdeaStarted   = "started"
	IdeaDismissed = "dismissed"
)

// Idea is a row of ideas. IDs use the same global scheme (IDEA-007).
type Idea struct {
	ID                 string
	Title              string
	Pitch              string
	Rationale          string
	Status             string
	StartedProjectSlug string
	CreatedAt          time.Time
	GeneratedBy        string
}

// Session is a chat session.
type Session struct {
	ID        string
	Title     string
	StartedAt time.Time
}

// Message roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// Message is one turn in a session.
type Message struct {
	ID           int64
	SessionID    string
	Role         string
	Content      string
	CreatedAt    time.Time
	Model        string
	InputTokens  *int
	OutputTokens *int
}

// Exec run exit statuses.
const (
	ExitSuccess   = "success"
	ExitFailed    = "failed"
	ExitCancelled = "cancelled"
)

// ExecRun records one dispatch to an executor.
type ExecRun struct {
	ID string
	// ActionItemID is empty for a dispatch with no item behind it, which is
	// what `apex start` does when it scaffolds a project from an idea.
	ActionItemID string
	// ProjectSlug is what was worked on. It is the only thing that says so
	// for a run with no action item.
	ProjectSlug string
	Executor    string
	// SessionID is the agent session Apex generated for this run, so a
	// stalled dispatch is resumable with `claude --resume` (DESIGN.md §9).
	SessionID  string
	StartedAt  time.Time
	FinishedAt *time.Time
	ExitStatus string
	LogPath    string
}
