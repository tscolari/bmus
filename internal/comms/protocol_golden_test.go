package comms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
)

const protocolDir = "../../protocol"

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join(protocolDir, "messages.schema.json")
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	require.NoError(t, err)

	c := jsonschema.NewCompiler()
	c.AssertFormat()
	require.NoError(t, c.AddResource(path, doc))
	sch, err := c.Compile(path)
	require.NoError(t, err)
	return sch
}

// validate checks raw JSON against the schema.
func validate(sch *jsonschema.Schema, raw []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return sch.Validate(inst)
}

// generic decodes raw JSON into plain maps/slices, keeping numbers exact.
func generic(t *testing.T, raw []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	require.NoError(t, d.Decode(&v))
	return v
}

func loadGolden(t *testing.T) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(protocolDir, "golden", "*.json"))
	require.NoError(t, err)
	golden := make(map[string][]byte, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		require.NoError(t, err)
		golden[strings.TrimSuffix(filepath.Base(p), ".json")] = raw
	}
	return golden
}

func TestGolden(t *testing.T) {
	sch := compileSchema(t)
	golden := loadGolden(t)

	// One golden file per message type, named after it, plus variants
	// named <type>.<variant> (answer.budget, #119).
	var types []string
	for typ := range bodyTypes {
		types = append(types, typ)
	}
	var names []string
	for name := range golden {
		if typ, _, variant := strings.Cut(name, "."); variant {
			require.Contains(t, types, typ, name)
			continue
		}
		names = append(names, name)
	}
	require.ElementsMatch(t, types, names)

	for name, raw := range golden {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, validate(sch, raw))

			var m Message
			require.NoError(t, json.Unmarshal(raw, &m))
			typ, _, _ := strings.Cut(name, ".")
			require.Equal(t, typ, m.Type)

			// Re-encode through the typed body so a field the Go struct
			// drops or renames shows up as a difference.
			body, err := m.DecodeBody()
			require.NoError(t, err)
			m.Body, err = json.Marshal(body)
			require.NoError(t, err)
			out, err := json.Marshal(m)
			require.NoError(t, err)

			require.Equal(t, generic(t, raw), generic(t, out))
			require.NoError(t, validate(sch, out))
		})
	}
}

func TestSchemaRejects(t *testing.T) {
	sch := compileSchema(t)
	golden := loadGolden(t)

	// mutate returns golden[name] with fn applied to its decoded envelope.
	mutate := func(name string, fn func(m, body map[string]any)) []byte {
		var m map[string]any
		require.NoError(t, json.Unmarshal(golden[name], &m))
		fn(m, m["body"].(map[string]any))
		raw, err := json.Marshal(m)
		require.NoError(t, err)
		return raw
	}

	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"missing seq", mutate("hail", func(m, _ map[string]any) { delete(m, "seq") })},
		{"negative seq", mutate("ack", func(m, _ map[string]any) { m["seq"] = -1 })},
		{"wrong version", mutate("hello", func(m, _ map[string]any) { m["v"] = 2 })},
		{"unknown type", mutate("hello", func(m, _ map[string]any) { m["type"] = "hullo" })},
		{"missing type", mutate("hello", func(m, _ map[string]any) { delete(m, "type") })},
		{"bad ts", mutate("heartbeat", func(m, _ map[string]any) { m["ts"] = "yesterday" })},
		{"extra envelope field", mutate("heartbeat", func(m, _ map[string]any) { m["extra"] = true })},
		{"extra body field", mutate("progress", func(_, b map[string]any) { b["eta"] = "soon" })},
		{"body of another type", mutate("ack", func(m, _ map[string]any) { m["body"] = map[string]any{"pct": 1, "note": ""} })},
		{"order-bound without order", mutate("report", func(m, _ map[string]any) { delete(m, "order") })},
		{"order-bound with empty order", mutate("answer", func(m, _ map[string]any) { m["order"] = "" })},
		{"hail with hail_id", mutate("hail", func(_, b map[string]any) { b["hail_id"] = "H-0091" })},
		{"hail without ref", mutate("hail", func(_, b map[string]any) { delete(b, "ref") })},
		{"pct over 100", mutate("progress", func(_, b map[string]any) { b["pct"] = 101 })},
		{"unknown decided_by", mutate("answer", func(_, b map[string]any) { b["decided_by"] = "ensign" })},
		{"answer without hail_id", mutate("answer", func(_, b map[string]any) { delete(b, "hail_id") })},
		{"correction without order", mutate("correction", func(m, _ map[string]any) { delete(m, "order") })},
		{"correction without note", mutate("correction", func(_, b map[string]any) { delete(b, "note") })},
		{"correction with an empty hail_id", mutate("correction", func(_, b map[string]any) { b["hail_id"] = "" })},
		{"correction with a decision", mutate("correction", func(_, b map[string]any) { b["decision"] = "deny" })},
		{"confidence over 1", mutate("consult_reply", func(_, b map[string]any) { b["confidence"] = 1.5 })},
		{"usage without order", mutate("usage", func(m, _ map[string]any) { delete(m, "order") })},
		{"usage without turns", mutate("usage", func(_, b map[string]any) { delete(b, "turns") })},
		{"usage with a seq", mutate("usage", func(_, b map[string]any) { b["seq"] = 2 })},
		{"usage with negative tokens", mutate("usage", func(_, b map[string]any) { b["input_tokens"] = -1 })},
		{"pct as string", mutate("progress", func(_, b map[string]any) { b["pct"] = "40" })},
		{"empty order on error", mutate("error", func(m, _ map[string]any) { m["order"] = "" })},
		{"checkpoint_ready without repos", mutate("checkpoint_ready", func(_, b map[string]any) { delete(b, "repos") })},
		{"checkpoint_ready with a top-level commit", mutate("checkpoint_ready", func(_, b map[string]any) { b["commit"] = "9f2c" })},
		{"checkpoint_ready repo without commit", mutate("checkpoint_ready", func(_, b map[string]any) {
			delete(b["repos"].([]any)[0].(map[string]any), "commit")
		})},
		{"checkpoint_ready with stopping as string", mutate("checkpoint_ready", func(_, b map[string]any) { b["stopping"] = "yes" })},
		{"checkpoint_ready with an empty held ref", mutate("checkpoint_ready.park", func(_, b map[string]any) { b["held"] = []any{""} })},
		{"checkpoint_ready with an empty full bundle path", mutate("checkpoint_ready.park", func(_, b map[string]any) {
			b["repos"].([]any)[0].(map[string]any)["full_bundle_path"] = ""
		})},
		{"report with branch", mutate("report", func(_, b map[string]any) { b["branch"] = "bmus/ORD-0414" })},
		{"report without repos", mutate("report", func(_, b map[string]any) { delete(b, "repos") })},
		{"report with null repos", mutate("report", func(_, b map[string]any) { b["repos"] = nil })},
		{"report repo without bundle_path", mutate("report", func(_, b map[string]any) {
			delete(b["repos"].([]any)[0].(map[string]any), "bundle_path")
		})},
		{"report repo without branch", mutate("report", func(_, b map[string]any) {
			delete(b["repos"].([]any)[0].(map[string]any), "branch")
		})},
		{"report repo named out", mutate("report", func(_, b map[string]any) { b["repos"].([]any)[0].(map[string]any)["name"] = "out" })},
		{"null body", mutate("belay", func(m, _ map[string]any) { m["body"] = nil })},
		{"beam seq 0", mutate("heartbeat", func(m, _ map[string]any) { m["seq"] = 0 })},
		{"order with base_ref", mutate("order", func(_, b map[string]any) { b["base_ref"] = "main" })},
		{"order with empty model", mutate("order", func(_, b map[string]any) { b["model"] = "" })},
		{"hello with empty model", mutate("hello", func(_, b map[string]any) { b["model"] = "" })},
		{"order with null handover", mutate("order", func(_, b map[string]any) { b["handover"] = nil })},
		{"order without repos", mutate("order", func(_, b map[string]any) { delete(b, "repos") })},
		{"order with null repos", mutate("order", func(_, b map[string]any) { b["repos"] = nil })},
		{"order repo without bundle_path", mutate("order", func(_, b map[string]any) {
			delete(b["repos"].([]any)[0].(map[string]any), "bundle_path")
		})},
		{"order repo named ..", mutate("order", func(_, b map[string]any) { b["repos"].([]any)[0].(map[string]any)["name"] = ".." })},
		{"order repo named in", mutate("order", func(_, b map[string]any) { b["repos"].([]any)[0].(map[string]any)["name"] = "in" })},
		{"order repo named out", mutate("order", func(_, b map[string]any) { b["repos"].([]any)[0].(map[string]any)["name"] = "out" })},
		{"order repo named 12", mutate("order", func(_, b map[string]any) { b["repos"].([]any)[0].(map[string]any)["name"] = "12" })},
		{"order repo name with a slash", mutate("order", func(_, b map[string]any) { b["repos"].([]any)[0].(map[string]any)["name"] = "a/b" })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, validate(sch, tc.raw))
		})
	}
}

func TestSchemaTypesMatchGo(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(protocolDir, "messages.schema.json"))
	require.NoError(t, err)
	var schema struct {
		Properties struct {
			Type struct {
				Enum []string `json:"enum"`
			} `json:"type"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(raw, &schema))
	var types []string
	for typ := range bodyTypes {
		types = append(types, typ)
	}
	require.ElementsMatch(t, types, schema.Properties.Type.Enum)
}

// Zero values the Go structs emit must still satisfy the schema.
func TestZeroBodiesValidate(t *testing.T) {
	sch := compileSchema(t)
	zero := 0
	for _, tc := range []struct {
		typ   string
		order string
		body  any
	}{
		{TypeProgress, "ORD-0001", ProgressBody{}},
		{TypeHeartbeat, "", HeartbeatBody{}},
		{TypeAck, "", AckBody{}},
		{TypeOrder, "ORD-0001", OrderBody{Title: "t"}},
		{TypeAnswer, "ORD-0001", AnswerBody{Ref: "h-1", HailID: "H-0001", DecidedBy: DecidedByCaptain, Decision: OptionAllow, Option: &zero}},
		{TypeBelay, "ORD-0001", BelayBody{}},
		{TypeReport, "ORD-0001", ReportBody{}},
		{TypeCheckpointReady, "ORD-0001", CheckpointReadyBody{TranscriptPath: "/t", HandoverPath: "/h"}},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			m, err := NewMessage(tc.typ, tc.body)
			require.NoError(t, err)
			m.Seq, m.Crew, m.Order = 1, "sorell", tc.order
			raw, err := json.Marshal(m)
			require.NoError(t, err)
			require.NoError(t, validate(sch, raw), string(raw))
		})
	}
}

func TestNewMessage(t *testing.T) {
	opt := 1
	m, err := NewMessage(TypeAnswer, &AnswerBody{Ref: "h-7", HailID: "H-0001", DecidedBy: DecidedByCaptain, Decision: "Stay on target", Option: &opt})
	require.NoError(t, err)
	require.Equal(t, Version, m.V)
	require.Equal(t, TypeAnswer, m.Type)
	require.False(t, m.TS.IsZero())
	m.Seq, m.Crew, m.Order = 1, "sorell", "ORD-0001"

	raw, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, validate(compileSchema(t), raw))

	body, err := m.DecodeBody()
	require.NoError(t, err)
	require.Equal(t, AnswerBody{Ref: "h-7", HailID: "H-0001", DecidedBy: DecidedByCaptain, Decision: "Stay on target", Option: &opt}, body)

	_, err = NewMessage(TypeAnswer, HailBody{})
	require.ErrorContains(t, err, "needs body AnswerBody")
	_, err = NewMessage(TypeAnswer, nil)
	require.Error(t, err)
	_, err = NewMessage("hullo", HelloBody{})
	require.ErrorContains(t, err, "unknown message type")
	_, err = Message{Type: "hullo"}.DecodeBody()
	require.ErrorContains(t, err, "unknown message type")
	for _, b := range []string{"", "null", " null "} {
		_, err = Message{Type: TypeBelay, Body: json.RawMessage(b)}.DecodeBody()
		require.ErrorContains(t, err, "has no body", "body %q", b)
	}
}

// An order_split hail (#155) carries options [allow, deny] and an
// OrderSplitContext the schema checks, unlike other hails' free context.
func TestSchemaOrderSplit(t *testing.T) {
	sch := compileSchema(t)
	split := func(fn func(body, ctx map[string]any)) []byte {
		yellow := 1
		body := map[string]any{}
		raw, err := json.Marshal(HailBody{Ref: "sdk-ORD-0001-1", Category: CategoryOrderSplit, Question: "Split ORD-0001 into 2 orders?",
			Options: []string{OptionAllow, OptionDeny}})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		raw, err = json.Marshal(OrderSplitContext{Parent: "ORD-0001", Children: []OrderSplitChild{
			{Title: "API", Body: "the API half", Division: "ops", Priority: &yellow}, {Title: "UI", Body: "the UI half", After: []int{0}},
		}})
		require.NoError(t, err)
		ctx := map[string]any{}
		require.NoError(t, json.Unmarshal(raw, &ctx))
		body["context"] = ctx
		fn(body, ctx)
		m, err := json.Marshal(map[string]any{"v": 1, "seq": 3, "crew": "vance", "order": "ORD-0001", "type": "hail",
			"ts": "2026-10-30T09:00:00Z", "body": body})
		require.NoError(t, err)
		return m
	}
	child := func(ctx map[string]any) map[string]any { return ctx["children"].([]any)[0].(map[string]any) }
	require.NoError(t, validate(sch, split(func(_, _ map[string]any) {})))
	require.NoError(t, validate(sch, split(func(_, ctx map[string]any) { delete(ctx, "parent") })))

	for _, tc := range []struct {
		name string
		fn   func(body, ctx map[string]any)
	}{
		{"no context", func(b, _ map[string]any) { delete(b, "context") }},
		{"no options", func(b, _ map[string]any) { delete(b, "options") }},
		{"options reversed", func(b, _ map[string]any) { b["options"] = []string{OptionDeny, OptionAllow} }},
		{"no children", func(_, c map[string]any) { c["children"] = []any{} }},
		{"eleven children", func(_, c map[string]any) {
			kids := make([]any, 11)
			for i := range kids {
				kids[i] = map[string]any{"title": "t", "body": "b"}
			}
			c["children"] = kids
		}},
		{"empty title", func(_, c map[string]any) { child(c)["title"] = "" }},
		{"no body", func(_, c map[string]any) { delete(child(c), "body") }},
		{"priority 3", func(_, c map[string]any) { child(c)["priority"] = 3 }},
		{"unknown child field", func(_, c map[string]any) { child(c)["assignee"] = "sorell" }},
		{"after not a list", func(_, c map[string]any) { child(c)["after"] = 0 }},
		{"after negative", func(_, c map[string]any) { child(c)["after"] = []int{-1} }},
		{"after repeated", func(_, c map[string]any) { child(c)["after"] = []int{0, 0} }},
		{"after not an index", func(_, c map[string]any) { child(c)["after"] = []string{"ORD-0001"} }},
		{"after 9", func(_, c map[string]any) { child(c)["after"] = []int{9} }},
		{"after ten items", func(_, c map[string]any) { child(c)["after"] = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9} }},
		{"unknown context field", func(_, c map[string]any) { c["note"] = "x" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, validate(sch, split(tc.fn)))
		})
	}
}

// An order_reorder hail (#148) carries options [allow, deny] and an
// OrderReorderContext the schema checks.
func TestSchemaOrderReorder(t *testing.T) {
	sch := compileSchema(t)
	reorder := func(fn func(body, ctx map[string]any)) []byte {
		body := map[string]any{}
		raw, err := json.Marshal(HailBody{Ref: "sdk-ORD-0001-1", Category: CategoryOrderReorder, Question: "Reorder 2 orders?",
			Options: []string{OptionAllow, OptionDeny}})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		raw, err = json.Marshal(OrderReorderContext{Reason: "the API moved", Changes: []OrderReorderChange{
			{Order: "ORD-0003", Onto: "order:ORD-0005"}, {Order: "ORD-0004", AfterAdd: []string{"ORD-0003"}, AfterRemove: []string{"ORD-0002"}},
		}})
		require.NoError(t, err)
		ctx := map[string]any{}
		require.NoError(t, json.Unmarshal(raw, &ctx))
		body["context"] = ctx
		fn(body, ctx)
		m, err := json.Marshal(map[string]any{"v": 1, "seq": 3, "crew": "vance", "order": "ORD-0001", "type": "hail",
			"ts": "2026-11-14T09:00:00Z", "body": body})
		require.NoError(t, err)
		return m
	}
	change := func(ctx map[string]any) map[string]any { return ctx["changes"].([]any)[0].(map[string]any) }
	require.NoError(t, validate(sch, reorder(func(_, _ map[string]any) {})))
	require.NoError(t, validate(sch, reorder(func(_, c map[string]any) { change(c)["onto"] = "main" })))

	for _, tc := range []struct {
		name string
		fn   func(body, ctx map[string]any)
	}{
		{"no context", func(b, _ map[string]any) { delete(b, "context") }},
		{"options reversed", func(b, _ map[string]any) { b["options"] = []string{OptionDeny, OptionAllow} }},
		{"no reason", func(_, c map[string]any) { delete(c, "reason") }},
		{"empty reason", func(_, c map[string]any) { c["reason"] = "" }},
		{"no changes", func(_, c map[string]any) { c["changes"] = []any{} }},
		{"eleven changes", func(_, c map[string]any) {
			changes := make([]any, 11)
			for i := range changes {
				changes[i] = map[string]any{"order": fmt.Sprintf("ORD-%04d", i+1), "onto": "main"}
			}
			c["changes"] = changes
		}},
		{"a change asking for nothing", func(_, c map[string]any) { delete(change(c), "onto") }},
		{"no order", func(_, c map[string]any) { delete(change(c), "order") }},
		{"not an order id", func(_, c map[string]any) { change(c)["order"] = "ord-3" }},
		{"empty onto", func(_, c map[string]any) { change(c)["onto"] = "" }},
		{"after_add not an id", func(_, c map[string]any) { change(c)["after_add"] = []string{"main"} }},
		{"after_remove repeated", func(_, c map[string]any) { change(c)["after_remove"] = []string{"ORD-0002", "ORD-0002"} }},
		{"unknown change field", func(_, c map[string]any) { change(c)["repo"] = "app" }},
		{"unknown context field", func(_, c map[string]any) { c["note"] = "x" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, validate(sch, reorder(tc.fn)))
		})
	}
}

// tracker_comment and tracker_close hails (#54) carry options [allow, deny]
// and a context the schema checks.
func TestSchemaTrackerRequests(t *testing.T) {
	sch := compileSchema(t)
	hail := func(category string, ctx any, fn func(body map[string]any)) []byte {
		body := map[string]any{}
		raw, err := json.Marshal(HailBody{Ref: "sdk-ORD-0001-1", Category: category, Question: "?",
			Options: []string{OptionAllow, OptionDeny}})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		raw, err = json.Marshal(ctx)
		require.NoError(t, err)
		c := map[string]any{}
		require.NoError(t, json.Unmarshal(raw, &c))
		body["context"] = c
		fn(body)
		m, err := json.Marshal(map[string]any{"v": 1, "seq": 3, "crew": "sorell", "order": "ORD-0001", "type": "hail",
			"ts": "2026-11-07T09:00:00Z", "body": body})
		require.NoError(t, err)
		return m
	}
	ok := func(map[string]any) {}
	comment := TrackerCommentContext{Ref: "o/r#12", Body: "Fixed in bmus/ORD-0001."}
	closing := TrackerCloseContext{Ref: "o/r#12", Reason: "not_planned"}
	require.NoError(t, validate(sch, hail(CategoryTrackerComment, comment, ok)))
	require.NoError(t, validate(sch, hail(CategoryTrackerClose, closing, ok)))
	require.NoError(t, validate(sch, hail(CategoryTrackerClose, TrackerCloseContext{Ref: "o/r#12"}, ok)))

	ctxOf := func(b map[string]any) map[string]any { return b["context"].(map[string]any) }
	for _, tc := range []struct {
		name     string
		category string
		ctx      any
		fn       func(body map[string]any)
	}{
		{"comment without context", CategoryTrackerComment, comment, func(b map[string]any) { delete(b, "context") }},
		{"comment without options", CategoryTrackerComment, comment, func(b map[string]any) { delete(b, "options") }},
		{"comment without body", CategoryTrackerComment, comment, func(b map[string]any) { delete(ctxOf(b), "body") }},
		{"comment with an empty ref", CategoryTrackerComment, comment, func(b map[string]any) { ctxOf(b)["ref"] = "" }},
		{"comment with a reason", CategoryTrackerComment, comment, func(b map[string]any) { ctxOf(b)["reason"] = "completed" }},
		{"close without ref", CategoryTrackerClose, closing, func(b map[string]any) { delete(ctxOf(b), "ref") }},
		{"close with a body", CategoryTrackerClose, closing, func(b map[string]any) { ctxOf(b)["body"] = "x" }},
		{"close as a duplicate", CategoryTrackerClose, closing, func(b map[string]any) { ctxOf(b)["reason"] = "duplicate" }},
		{"close options reversed", CategoryTrackerClose, closing, func(b map[string]any) { b["options"] = []string{OptionDeny, OptionAllow} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, validate(sch, hail(tc.category, tc.ctx, tc.fn)))
		})
	}
}

// A budget hail (#119) carries options [extend, stop] and a BudgetContext;
// an answer that extends it carries the new limit.
func TestSchemaBudget(t *testing.T) {
	t.Parallel()
	sch := compileSchema(t)
	hail := func(fn func(body, ctx map[string]any)) []byte {
		body := map[string]any{}
		raw, err := json.Marshal(HailBody{Ref: "sdk-ORD-0001-3", Category: CategoryBudget, Question: "Over budget: extend or stop?",
			Options: []string{OptionExtend, OptionStop}})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		raw, err = json.Marshal(BudgetContext{Used: 1_200_000, Limit: 1_000_000, TurnsUsed: 140, TurnsLimit: 150})
		require.NoError(t, err)
		ctx := map[string]any{}
		require.NoError(t, json.Unmarshal(raw, &ctx))
		body["context"] = ctx
		fn(body, ctx)
		m, err := json.Marshal(map[string]any{"v": 1, "seq": 3, "crew": "sorell", "order": "ORD-0001", "type": "hail",
			"ts": "2026-11-13T09:00:00Z", "body": body})
		require.NoError(t, err)
		return m
	}
	require.NoError(t, validate(sch, hail(func(_, _ map[string]any) {})))
	for _, tc := range []struct {
		name string
		fn   func(body, ctx map[string]any)
	}{
		{"no context", func(b, _ map[string]any) { delete(b, "context") }},
		{"no options", func(b, _ map[string]any) { delete(b, "options") }},
		{"allow and deny", func(b, _ map[string]any) { b["options"] = []string{OptionAllow, OptionDeny} }},
		{"options reversed", func(b, _ map[string]any) { b["options"] = []string{OptionStop, OptionExtend} }},
		{"no turns_limit", func(_, c map[string]any) { delete(c, "turns_limit") }},
		{"negative used", func(_, c map[string]any) { c["used"] = -1 }},
		{"unknown context field", func(_, c map[string]any) { c["note"] = "x" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, validate(sch, hail(tc.fn)))
		})
	}

	zero := 0
	m, err := NewMessage(TypeAnswer, AnswerBody{Ref: "sdk-ORD-0001-3", HailID: "H-0007", DecidedBy: DecidedByCaptain,
		Decision: OptionExtend, Option: &zero, Budget: &BudgetLimit{Tokens: 2_000_000, Turns: 300}})
	require.NoError(t, err)
	m.Seq, m.Crew, m.Order = 9, "sorell", "ORD-0001"
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, validate(sch, raw), string(raw))
	require.Contains(t, string(raw), `"budget":{"tokens":2000000,"turns":300}`)
	bad := strings.Replace(string(raw), `"budget":{"tokens":2000000,"turns":300}`, `"budget":{"tokens":2000000}`, 1)
	require.Error(t, validate(sch, []byte(bad)))
}

// A finding (#153) is order-bound and its body holds the plan's limits.
// FindingBody.Check, which the pump runs since nothing validates against
// the schema at runtime, agrees with the schema on every case.
func TestSchemaFinding(t *testing.T) {
	t.Parallel()
	sch := compileSchema(t)
	long := func(n int) string { return strings.Repeat("x", n) }
	keys := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("k%d", i)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		body FindingBody
		ok   bool
	}{
		{"minimal", FindingBody{Key: "k", Title: "t"}, true},
		{"full", FindingBody{Key: "snapshot-leak", Title: "Leak", Body: "b", Labels: []string{"bug"}, Severity: "low",
			BlockedBy: []string{"flaky-test", "#12", "example/app#40", "12"}}, true},
		{"limits", FindingBody{Key: "a" + long(39), Title: strings.Repeat("漢", MaxFindingTitle), Body: long(MaxFindingBody),
			Labels: []string{long(50), "b", "c", "d", "e"}, Severity: "high", BlockedBy: keys(MaxFindingBlockedBy)}, true},
		{"empty key", FindingBody{Title: "t"}, false},
		{"key with capitals", FindingBody{Key: "Leak", Title: "t"}, false},
		{"key with a leading dash", FindingBody{Key: "-leak", Title: "t"}, false},
		{"key over 40", FindingBody{Key: long(41), Title: "t"}, false},
		{"empty title", FindingBody{Key: "k"}, false},
		{"blank title", FindingBody{Key: "k", Title: "  "}, false},
		{"two-line title", FindingBody{Key: "k", Title: "a\nb"}, false},
		{"title over 200", FindingBody{Key: "k", Title: long(201)}, false},
		{"six labels", FindingBody{Key: "k", Title: "t", Labels: []string{"a", "b", "c", "d", "e", "f"}}, false},
		{"empty label", FindingBody{Key: "k", Title: "t", Labels: []string{""}}, false},
		{"label with a comma", FindingBody{Key: "k", Title: "t", Labels: []string{"a,b"}}, false},
		{"label over 50", FindingBody{Key: "k", Title: "t", Labels: []string{long(51)}}, false},
		{"title with an escape", FindingBody{Key: "k", Title: "t\x1b[2J"}, false},
		{"title with a tab", FindingBody{Key: "k", Title: "a\tb"}, false},
		{"title with DEL", FindingBody{Key: "k", Title: "a\x7f"}, false},
		{"title with a NUL", FindingBody{Key: "k", Title: "a\x00"}, false},
		{"blank label", FindingBody{Key: "k", Title: "t", Labels: []string{"  "}}, false},
		{"label with a leading space", FindingBody{Key: "k", Title: "t", Labels: []string{" bug"}}, false},
		{"label with a trailing space", FindingBody{Key: "k", Title: "t", Labels: []string{"bug "}}, false},
		{"label with a control character", FindingBody{Key: "k", Title: "t", Labels: []string{"a\x1bb"}}, false},
		{"reserved label", FindingBody{Key: "k", Title: "t", Labels: []string{"bmus:unreviewed"}}, false},
		{"reserved label in capitals", FindingBody{Key: "k", Title: "t", Labels: []string{"BMUS:Kept"}}, false},
		{"label with spaces inside", FindingBody{Key: "k", Title: "t", Labels: []string{"good first issue", "x", "bmus"}}, true},
		{"label naming bmus later", FindingBody{Key: "k", Title: "t", Labels: []string{"area:bmus:x"}}, true},
		{"unknown severity", FindingBody{Key: "k", Title: "t", Severity: "critical"}, false},
		{"eleven blockers", FindingBody{Key: "k", Title: "t", BlockedBy: keys(11)}, false},
		{"blocker neither key nor ref", FindingBody{Key: "k", Title: "t", BlockedBy: []string{"owner/repo"}}, false},
		{"blocker ref without a number", FindingBody{Key: "k", Title: "t", BlockedBy: []string{"#"}}, false},
		{"blocker ref with a long repo", FindingBody{Key: "k", Title: "t", BlockedBy: []string{long(150) + "/" + long(50) + "#1"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := NewMessage(TypeFinding, tc.body)
			require.NoError(t, err)
			m.Seq, m.Crew, m.Order = 1, "sorell", "ORD-0001"
			raw, err := json.Marshal(m)
			require.NoError(t, err)
			if tc.ok {
				require.NoError(t, validate(sch, raw))
				require.NoError(t, tc.body.Check())
			} else {
				require.Error(t, validate(sch, raw))
				require.Error(t, tc.body.Check())
			}
		})
	}

	// The body's cap is in bytes for the bridge, characters for the schema.
	cjk := FindingBody{Key: "k", Title: "t", Body: strings.Repeat("漢", 11_000)}
	require.Error(t, cjk.Check())

	golden := loadGolden(t)
	var m map[string]any
	require.NoError(t, json.Unmarshal(golden["finding"], &m))
	delete(m, "order")
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	require.Error(t, validate(sch, raw), "a finding is order-bound")
}
