// Package comms carries messages between the bridge and a crew member's
// combadge: newline-delimited JSON over a bridge-initiated session (spec D3).
//
// protocol.go mirrors protocol/messages.schema.json, the source of truth shared
// with the TypeScript combadge. Keep the two in step: the golden tests in
// protocol_golden_test.go validate every protocol/golden/*.json against the
// schema and round-trip it through these structs.
package comms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// Version is the protocol version carried in every envelope's v field.
const Version = 1

// Beam->bridge message types. Seq on these is the combadge's outbox seq: it
// starts at 1 and is monotonic for the life of the outbox file, not per
// process, so a restarted combadge resumes from the outbox.
const (
	TypeHello           = "hello"
	TypeHeartbeat       = "heartbeat"
	TypeProgress        = "progress"
	TypeUsage           = "usage"
	TypeHail            = "hail"
	TypeCheckpointReady = "checkpoint_ready"
	TypeReport          = "report"
	TypeError           = "error"
	TypeConsultReply    = "consult_reply" // Command lane only; unused in Phase 1.
)

// Bridge->beam message types. Seq on these is the command id the combadge
// dedups on, except for ack: acks are idempotent and always applied.
const (
	TypeAck        = "ack"
	TypeOrder      = "order"
	TypeAnswer     = "answer"
	TypeConsult    = "consult" // Command lane only; unused in Phase 1.
	TypeCheckpoint = "checkpoint"
	TypeStandDown  = "stand_down"
	TypeRecall     = "recall"
	TypeBelay      = "belay"
	TypeCorrection = "correction"
)

// Who decided a hail (AnswerBody.DecidedBy).
const (
	DecidedByPolicy  = "policy"
	DecidedByCommand = "command"
	DecidedByCaptain = "captain"
)

// Hail categories the combadge emits. HailBody.Category is an open
// string so later categories (scope_change, destructive, egress, ...) fit.
const (
	CategoryClarifyingQuestion = "clarifying_question"
	CategoryToolApproval       = "tool_approval"
	// CategoryOrderSplit is Command's request to split its order into
	// child orders (#155): Options {OptionAllow, OptionDeny} and an
	// OrderSplitContext. An allowed split's answer has Option 0 and the
	// created orders' ids, comma separated, as its Decision.
	CategoryOrderSplit = "order_split"
	// CategoryTrackerComment and CategoryTrackerClose are a crew member's
	// requests to comment on or close a tracker item (#54): Options
	// {OptionAllow, OptionDeny} and a TrackerCommentContext or
	// TrackerCloseContext. The bridge runs an allowed one, then answers
	// with Option 0 and what it did as the Decision, or Option 1 and why
	// it couldn't.
	CategoryTrackerComment = "tracker_comment"
	CategoryTrackerClose   = "tracker_close"
)

// TrackerCommentContext is a tracker_comment hail's context: the item's
// ref and the comment.
type TrackerCommentContext struct {
	Ref  string `json:"ref"`
	Body string `json:"body"`
}

// TrackerCloseContext is a tracker_close hail's context: the item's ref
// and, optionally, why it closes: completed (the default) or
// not_planned.
type TrackerCloseContext struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason,omitempty"`
}

// OrderSplitContext is an order_split hail's context. Parent, when set,
// is the hailing order's id.
type OrderSplitContext struct {
	Parent   string            `json:"parent,omitempty"`
	Children []OrderSplitChild `json:"children"`
}

// OrderSplitChild is one child order of an order_split hail. An empty
// Division and a nil Priority take the parent order's. Priority is 0
// routine, 1 yellow or 2 red.
type OrderSplitChild struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Division string `json:"division,omitempty"`
	Priority *int   `json:"priority,omitempty"`
}

// Options of a tool_approval hail, in this order. AnswerBody.Option 0
// (OptionAllow) allows the tool; any other option, or none, denies it.
const (
	OptionAllow = "allow"
	OptionDeny  = "deny"
)

// Message is the envelope of every comms line.
//
// Seq is per direction and the two sequences are independent: on beam->bridge
// messages it is the combadge outbox seq (from 1, monotonic across combadge
// restarts), which the bridge records in the inbox table before acking; on
// bridge->beam commands it is the command id the combadge dedups on (acks
// excepted, they are idempotent). Order is empty for messages not tied to an order (hello,
// heartbeat, ack, and optionally error, stand_down, consult, consult_reply);
// the schema requires it for every other type.
type Message struct {
	V     int             `json:"v"`
	Seq   uint64          `json:"seq"`
	Crew  string          `json:"crew"`
	Order string          `json:"order,omitempty"`
	Type  string          `json:"type"`
	TS    time.Time       `json:"ts"`
	Body  json.RawMessage `json:"body"`
}

// bodyTypes maps each message type to its body struct.
var bodyTypes = map[string]reflect.Type{
	TypeHello:           reflect.TypeFor[HelloBody](),
	TypeHeartbeat:       reflect.TypeFor[HeartbeatBody](),
	TypeProgress:        reflect.TypeFor[ProgressBody](),
	TypeUsage:           reflect.TypeFor[UsageBody](),
	TypeHail:            reflect.TypeFor[HailBody](),
	TypeCheckpointReady: reflect.TypeFor[CheckpointReadyBody](),
	TypeReport:          reflect.TypeFor[ReportBody](),
	TypeError:           reflect.TypeFor[ErrorBody](),
	TypeConsultReply:    reflect.TypeFor[ConsultReplyBody](),
	TypeAck:             reflect.TypeFor[AckBody](),
	TypeOrder:           reflect.TypeFor[OrderBody](),
	TypeAnswer:          reflect.TypeFor[AnswerBody](),
	TypeConsult:         reflect.TypeFor[ConsultBody](),
	TypeCheckpoint:      reflect.TypeFor[CheckpointBody](),
	TypeStandDown:       reflect.TypeFor[StandDownBody](),
	TypeRecall:          reflect.TypeFor[RecallBody](),
	TypeBelay:           reflect.TypeFor[BelayBody](),
	TypeCorrection:      reflect.TypeFor[CorrectionBody](),
}

// NewMessage builds a message of type typ with body marshalled into Body, V set
// to Version and TS to now (UTC). body must be the type's body struct (or a
// pointer to it). The caller fills in Seq, Crew and Order.
func NewMessage(typ string, body any) (Message, error) {
	want, ok := bodyTypes[typ]
	if !ok {
		return Message{}, fmt.Errorf("comms: unknown message type %q", typ)
	}
	if got := reflect.Indirect(reflect.ValueOf(body)); !got.IsValid() || got.Type() != want {
		return Message{}, fmt.Errorf("comms: %s message needs body %s, got %T", typ, want.Name(), body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Message{}, fmt.Errorf("comms: marshal %s body: %w", typ, err)
	}
	return Message{V: Version, Type: typ, TS: time.Now().UTC(), Body: raw}, nil
}

// DecodeBody unmarshals Body into the struct for m.Type and returns it by value
// (e.g. HailBody), ready for a type switch. An empty or null body is an
// error. Unknown fields are ignored here; strictness lives in the schema and
// the golden tests.
func (m Message) DecodeBody() (any, error) {
	t, ok := bodyTypes[m.Type]
	if !ok {
		return nil, fmt.Errorf("comms: unknown message type %q", m.Type)
	}
	if b := bytes.TrimSpace(m.Body); len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil, fmt.Errorf("comms: %s message has no body", m.Type)
	}
	p := reflect.New(t)
	if err := json.Unmarshal(m.Body, p.Interface()); err != nil {
		return nil, fmt.Errorf("comms: decode %s body: %w", m.Type, err)
	}
	return p.Elem().Interface(), nil
}

// HelloBody opens each attach session.
type HelloBody struct {
	Protocol        int    `json:"protocol"`
	Harness         string `json:"harness"`
	CombadgeVersion string `json:"combadge_version"`
	// Model is the harness's default model, the one an order without a
	// model runs on; empty when the harness doesn't know it.
	Model string `json:"model,omitempty"`
}

// HeartbeatBody reports beam health; missing beats trigger the sickbay probe.
type HeartbeatBody struct {
	DiskFreeBytes uint64 `json:"disk_free_bytes"`
	MemFreeBytes  uint64 `json:"mem_free_bytes"`
	OutboxDepth   uint64 `json:"outbox_depth"` // outbox entries not yet acked
}

// ProgressBody is agent-reported progress; it goes straight to the log.
type ProgressBody struct {
	Pct  float64 `json:"pct"` // 0-100
	Note string  `json:"note"`
}

// UsageBody is the running usage of an order on this beam (#117),
// cumulative over every run and query of the order on this beam, combadge
// restarts included. InputTokens counts cache reads and writes. Turns
// counts main-loop turns only. Counts never decrease, and the latest report
// for (order, beam) is the one with the highest envelope Seq, which is
// monotonic across combadge restarts. The harness sends one every 10 turns
// and at every checkpoint, stop and report.
type UsageBody struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	Turns        uint64 `json:"turns"`
}

// HailBody is a question or permission request the agent is blocked on.
//
// It deliberately carries no hail_id (unlike the spec §6 example): the bridge
// generates hail ids in store.InsertHail. Ref is the combadge's own id for the
// hail, unique per combadge process, and comes back in AnswerBody.Ref.
//
// A CategoryToolApproval hail carries Options {OptionAllow, OptionDeny}. A
// CategoryClarifyingQuestion hail carries one question, with or without
// options. Context must be a JSON object or nil.
type HailBody struct {
	Ref      string          `json:"ref"`
	Category string          `json:"category"`
	Question string          `json:"question"`
	Options  []string        `json:"options,omitempty"` // empty means free-text answer
	Context  json.RawMessage `json:"context,omitempty"` // JSON object or nil
}

// CheckpointReadyBody names a checkpoint on the beam's disk for the bridge to
// pull into the cargo bay: one bundle per repo of the order, the session
// transcript and the handover note. Paths are absolute on the beam. Never
// null on the wire: nil Repos marshals as [].
type CheckpointReadyBody struct {
	Repos          []WorkRepo `json:"repos"`
	TranscriptPath string     `json:"transcript_path"`
	HandoverPath   string     `json:"handover_path"`
	// Stopping marks the checkpoint that answers a stand_down or recall:
	// the harness stops after it, and the order ends with no report or
	// error.
	Stopping bool `json:"stopping,omitempty"`
}

// MarshalJSON writes a nil Repos as [], which the schema requires.
func (b CheckpointReadyBody) MarshalJSON() ([]byte, error) {
	type plain CheckpointReadyBody
	if b.Repos == nil {
		b.Repos = []WorkRepo{}
	}
	return json.Marshal(plain(b))
}

// ReportBody is the final result of an order. Repos holds the work, one
// bundle per repo of the order, for the bridge to pull and fetch into its
// clone before it marks the order done (#53). Never null on the wire: nil
// Repos marshals as [].
type ReportBody struct {
	Summary   string     `json:"summary"`
	Repos     []WorkRepo `json:"repos"`
	Artifacts []string   `json:"artifacts,omitempty"`
	Usage     Usage      `json:"usage"`
}

// MarshalJSON writes a nil Repos as [], which the schema requires.
func (b ReportBody) MarshalJSON() ([]byte, error) {
	type plain ReportBody
	if b.Repos == nil {
		b.Repos = []WorkRepo{}
	}
	return json.Marshal(plain(b))
}

// WorkRepo is the work in one repo of an order: a git bundle at BundlePath
// (absolute on the beam) carrying Branch, bmus/<order>, at Commit. The
// bundle holds the branch minus the order's base commit, or its whole
// history when the branch has no commits past it.
type WorkRepo struct {
	Name       string `json:"name"`
	BundlePath string `json:"bundle_path"`
	Commit     string `json:"commit"`
	Branch     string `json:"branch"`
}

// Usage is token usage for an order.
type Usage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

// ErrorBody reports a harness crash, tool failure or exceeded budget.
type ErrorBody struct {
	Reason string `json:"reason"`
	Code   string `json:"code,omitempty"` // e.g. budget_exceeded, exit_137
}

// ConsultReplyBody is Command's structured decision on a consulted hail
// (spec D7). Command lane only; unused in Phase 1.
type ConsultReplyBody struct {
	HailID     string   `json:"hail_id"`
	Decision   string   `json:"decision"`         // answer or escalate
	Option     *int     `json:"option,omitempty"` // index into the hail's options
	Answer     string   `json:"answer,omitempty"` // free text when there are no options
	Rationale  string   `json:"rationale"`
	Confidence float64  `json:"confidence"` // 0-1
	RiskFlags  []string `json:"risk_flags,omitempty"`
}

// AckBody lets the combadge trim its outbox.
type AckBody struct {
	Seq uint64 `json:"seq"` // highest contiguous beam->bridge seq recorded; 0 means none
}

// OrderBody assigns the mission named by the envelope's Order.
type OrderBody struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Repos are the repos the order works on, shipped onto the beam before
	// the order. Never null on the wire: nil marshals as [].
	Repos        []OrderRepo `json:"repos"`
	BudgetTokens uint64      `json:"budget_tokens"` // 0 means no budget
	Dossier      string      `json:"dossier,omitempty"`
	// Model is the model to run the order on: the order's own, else the
	// crew member's, else its division's default. It is opaque to the
	// bridge; empty means the harness's default.
	Model string `json:"model,omitempty"`
	// Handover is the note of the checkpoint the order resumes from; each
	// repo's bundle then carries that checkpoint's commits. Empty on a fresh
	// order.
	Handover string `json:"handover,omitempty"`
}

// MarshalJSON writes a nil Repos as [], which the schema requires.
func (b OrderBody) MarshalJSON() ([]byte, error) {
	type plain OrderBody
	if b.Repos == nil {
		b.Repos = []OrderRepo{}
	}
	return json.Marshal(plain(b))
}

// OrderRepo is one repo of an order. BundlePath is the absolute path on the
// beam of a git bundle holding branch bmus/<order> at BaseRef's commit, with
// its full history; combadge clones it into <work-dir>/<order>/<Name> and
// checks out bmus/<order> before the harness starts.
type OrderRepo struct {
	Name       string `json:"name"`
	BaseRef    string `json:"base_ref"`
	BundlePath string `json:"bundle_path"`
}

// AnswerBody is the decision on a hail. Ref echoes HailBody.Ref; HailID is the
// bridge's id for it.
//
// For a tool_approval hail, Option 0 (OptionAllow) allows the tool; any other
// Option, or nil, denies it with Decision as the deny message. For a
// clarifying_question hail, Decision is the answer text and Option is set when
// a listed option was picked.
type AnswerBody struct {
	Ref       string `json:"ref"`
	HailID    string `json:"hail_id"`
	DecidedBy string `json:"decided_by"` // DecidedByPolicy, DecidedByCommand or DecidedByCaptain
	Decision  string `json:"decision"`
	Option    *int   `json:"option,omitempty"` // index into the hail's options, if one was picked
	Rationale string `json:"rationale,omitempty"`
}

// ConsultBody is a hail plus the context Command cannot see on its own
// (spec D7). Command lane only; unused in Phase 1.
type ConsultBody struct {
	HailID      string          `json:"hail_id"`
	Crew        string          `json:"crew"`  // who hailed
	Order       string          `json:"order"` // the hailing crew's order
	Category    string          `json:"category"`
	Question    string          `json:"question"`
	Options     []string        `json:"options,omitempty"`
	Context     json.RawMessage `json:"context,omitempty"` // JSON object or nil
	OrderSpec   string          `json:"order_spec,omitempty"`
	RecentLog   []string        `json:"recent_log,omitempty"`
	Guidance    string          `json:"guidance,omitempty"`
	Regulations string          `json:"regulations,omitempty"`
	// Model is the model Command answers on: its crew member's, else its
	// division's default. It is opaque to the bridge; empty means the
	// harness's default.
	Model string `json:"model,omitempty"`
}

// CheckpointBody asks the combadge to reach a safe point and checkpoint.
type CheckpointBody struct {
	Reason string `json:"reason,omitempty"`
}

// StandDownBody ends the tour on this beam (watch change).
type StandDownBody struct {
	Reason string `json:"reason,omitempty"`
}

// RecallBody asks for a checkpoint and returns the order to the pool.
type RecallBody struct {
	Reason string `json:"reason,omitempty"`
}

// BelayBody abandons the order without checkpointing.
type BelayBody struct {
	Reason string `json:"reason,omitempty"`
}

// CorrectionBody tells a running order that the captain countermanded a
// decision made without them (Command's, or a captain timeout's
// default_option) after the agent already had it (phase 3 plan section
// 6). It is not a second answer. Ref is the order the hail belongs to, as
// the envelope's Order; Note is the new decision and the captain's reason.
// Question is the hail's, filled by the bridge when it sends the
// correction (#190), so a run that never got the first answer (one
// resumed after a park) can still name what was overridden.
type CorrectionBody struct {
	HailID   string `json:"hail_id"`
	Ref      string `json:"ref"`
	Note     string `json:"note"`
	Question string `json:"question,omitempty"`
}
