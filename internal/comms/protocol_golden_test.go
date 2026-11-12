package comms

import (
	"bytes"
	"encoding/json"
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

	// One golden file per message type, named after it.
	var types []string
	for typ := range bodyTypes {
		types = append(types, typ)
	}
	var names []string
	for name := range golden {
		names = append(names, name)
	}
	require.ElementsMatch(t, types, names)

	for name, raw := range golden {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, validate(sch, raw))

			var m Message
			require.NoError(t, json.Unmarshal(raw, &m))
			require.Equal(t, name, m.Type)

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
		{"pct as string", mutate("progress", func(_, b map[string]any) { b["pct"] = "40" })},
		{"empty order on error", mutate("error", func(m, _ map[string]any) { m["order"] = "" })},
		{"checkpoint_ready without repos", mutate("checkpoint_ready", func(_, b map[string]any) { delete(b, "repos") })},
		{"checkpoint_ready with a top-level commit", mutate("checkpoint_ready", func(_, b map[string]any) { b["commit"] = "9f2c" })},
		{"checkpoint_ready repo without commit", mutate("checkpoint_ready", func(_, b map[string]any) {
			delete(b["repos"].([]any)[0].(map[string]any), "commit")
		})},
		{"checkpoint_ready with stopping as string", mutate("checkpoint_ready", func(_, b map[string]any) { b["stopping"] = "yes" })},
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
			{Title: "API", Body: "the API half", Division: "ops", Priority: &yellow}, {Title: "UI", Body: "the UI half"},
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
		{"unknown context field", func(_, c map[string]any) { c["note"] = "x" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, validate(sch, split(tc.fn)))
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
