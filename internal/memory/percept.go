package memory

import "time"

// Producer identifies who created a Percept.
type Producer string

const (
	ProducerUser       Producer = "user"
	ProducerLocal      Producer = "primary"
	ProducerEscalation Producer = "escalation"
	ProducerSystem     Producer = "system"
)

// Consumer identifies which agent(s) should receive a Percept at injection time.
// Empty (ConsumerAll) is the default and means both agents receive it.
type Consumer string

const (
	ConsumerAll        Consumer = ""           // default — injected for both agents
	ConsumerLocal      Consumer = "primary"    // only injected for the primary agent
	ConsumerEscalation Consumer = "escalation" // only injected for the escalation agent
)

// Roles holds Neo-Davidsonian semantic roles extracted from a Percept's content.
// Fields left empty when not inferable — never fabricated.
type Roles struct {
	Action    string `json:"action,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Recipient string `json:"recipient,omitempty"`
	Theme     string `json:"theme,omitempty"`
	When      string `json:"when,omitempty"`
	Where     string `json:"where,omitempty"`
}

// Percept is an atomic memory unit: a single natural-language assertion with
// provenance, a confidence weight, and optional semantic roles.
type Percept struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Producer  Producer  `json:"producer"`
	Consumer  Consumer  `json:"consumer,omitempty"` // which agent(s) receive this at injection; empty = all
	W         float64   `json:"w"`                  // confidence weight [0,1]
	Roles     Roles     `json:"roles"`
	EngramID  string    `json:"engram_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Core      bool      `json:"core,omitempty"` // exempt from decay when true
	// LastUsedAt is when the Percept was last put to use: re-stated
	// (reinforced), returned by get_memory, or injected into a prompt. Zero
	// until first use after the field was introduced.
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	// IdleSeconds is how long milk has been running since the Percept was
	// last used — summed over sessions, so one 20-day session counts like ten
	// 2-day ones. Only global Percepts accrue it; it drives archiving of stale
	// global Percepts (see archive.go).
	IdleSeconds float64 `json:"idle_seconds,omitempty"`
	// Evidence counts corroboration events (near-duplicate reinforcement and
	// positive-edge boosts), awarded at most once per Store lifetime so
	// Evidence >= 2 means the fact held up in at least two distinct runs
	// (ADR-0090). Drives the promotion gate and the rank score.
	Evidence int `json:"evidence,omitempty"`
}

// Engram groups Percepts that share a subject. CompositeW is the mean weight
// of member Percepts, recomputed at consolidation.
type Engram struct {
	ID           string    `json:"id"`
	SubjectLabel string    `json:"subject_label"`
	CompositeW   float64   `json:"composite_w"`
	PerceptIDs   []string  `json:"percept_ids"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Relation classifies the semantic relationship between two Percepts.
type Relation string

const (
	RelationExtends     Relation = "extends"
	RelationUpdates     Relation = "updates"
	RelationDerives     Relation = "derives"
	RelationContradicts Relation = "contradicts"
)

// Edge records a typed directed relationship between two Percepts.
type Edge struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Relation Relation `json:"relation"`
}
