package http

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"finnri/internal/ai"
	"finnri/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"
)

// batchFixtureParser answers ParseTextBatch with `batch` and ParseText with
// `single`, and records which one the handler called.
type batchFixtureParser struct {
	batch  []byte
	single []byte
	calls  *[]string
}

func (p batchFixtureParser) Transcribe(context.Context, string, []byte, string) (string, error) {
	return "", nil
}

func (p batchFixtureParser) ParseText(context.Context, string, string, ai.KnownNames) ([]byte, ai.Usage, error) {
	*p.calls = append(*p.calls, "single")
	return p.single, ai.Usage{}, nil
}

func (p batchFixtureParser) ParseTextBatch(context.Context, string, string, ai.KnownNames) ([]byte, ai.Usage, error) {
	*p.calls = append(*p.calls, "batch")
	return p.batch, ai.Usage{}, nil
}

const singleMetroDraft = `{"title":"Metro","amount":45,"type":"expense","currency":"INR","mode":"UPI","category":"Travel","date":"2026-11-18"}`

func batchItem(title string, amount int, mode, slot string, carried bool) string {
	return fmt.Sprintf(`{"title":%q,"amount":%d,"type":"expense","currency":"INR","mode":%q,"category":"Food & Drinks","date":"2026-11-18","time_slot":%q,"mode_carried":%t,"source_text":%q}`,
		title, amount, mode, slot, carried, strings.ToLower(title)+fmt.Sprintf(" %d", amount))
}

func newBatchTestServer(t *testing.T, batch, single string) (*Server, *[]string) {
	t.Helper()
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	return &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata", MaxTranscriptChars: 1000, MaxTranscriptCharsBatch: 3000},
		validator: schema,
		parser:    batchFixtureParser{batch: []byte(batch), single: []byte(single), calls: &calls},
	}, &calls
}

func runBatchParse(t *testing.T, server *Server, text string, multi bool) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	form := url.Values{"hint_text": {text}}
	if multi {
		form.Set("multi", "1")
	}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	attachParseCreditUser(t, ctx)
	server.handleParse(ctx)
	body := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	return response, body
}

func TestParseBatchReturnsItemsChargedOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	batch := `{"stage":"batch","intent":"capture","over_cap_text":null,"items":[` +
		batchItem("Breakfast", 180, "UPI", "morning", false) + `,` +
		batchItem("Auto", 60, "UPI", "Morning", true) + `,` +
		batchItem("Lunch", 450, "Credit Card", "lunchtime", false) + `]}`
	server, calls := newBatchTestServer(t, batch, singleMetroDraft)

	response, body := runBatchParse(t, server, "breakfast 180 on upi then auto 60, lunch 450 on card", true)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(*calls) != 1 || (*calls)[0] != "batch" {
		t.Fatalf("parser calls = %v, want one batch call", *calls)
	}
	if body["kind"] != "batch" || body["stage"] != "batch" {
		t.Fatalf("not a batch: %s", response.Body.String())
	}
	if body["credits_charged"] != float64(5) {
		t.Fatalf("credits_charged = %v, want the text price once", body["credits_charged"])
	}
	items, _ := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3: %s", len(items), response.Body.String())
	}
	second := items[1].(map[string]any)
	if second["time_slot"] != "morning" || second["mode_carried"] != true {
		t.Fatalf("batch extras not kept: %v", second)
	}
	if second["source_text"] != "auto 60" {
		t.Fatalf("item source_text = %v, want its own words", second["source_text"])
	}
	if second["stage"] != "draft" {
		t.Fatalf("item was not normalised as a draft: %v", second)
	}
	// The prompt asks for lean items; each still reaches the app with every
	// key a single draft has.
	for _, key := range []string{"merchant", "account_hint", "split_candidate_details", "subscription_candidate", "tags", "needs_confirmation", "clarifications"} {
		if _, present := second[key]; !present {
			t.Fatalf("lean item is missing %q after the server filled it: %v", key, second)
		}
	}
	third := items[2].(map[string]any)
	if third["time_slot"] != nil {
		t.Fatalf("unknown slot should be null, got %v", third["time_slot"])
	}
	if body["ai_request_id"] == nil || body["source_text"] == nil {
		t.Fatalf("batch is missing request id or source text: %s", response.Body.String())
	}
}

func TestParseWithoutMultiKeepsTheSingleRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server, calls := newBatchTestServer(t, `{"stage":"batch","items":[]}`, singleMetroDraft)

	response, body := runBatchParse(t, server, "metro 45 via upi", false)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(*calls) != 1 || (*calls)[0] != "single" {
		t.Fatalf("parser calls = %v, want the single read for an old app", *calls)
	}
	if body["title"] != "Metro" || body["items"] != nil {
		t.Fatalf("old app got something other than a single draft: %s", response.Body.String())
	}
}

func TestParseBatchOfOneIsASingleDraft(t *testing.T) {
	gin.SetMode(gin.TestMode)
	batch := `{"stage":"batch","items":[` + batchItem("Lunch", 450, "UPI", "afternoon", false) + `]}`
	server, _ := newBatchTestServer(t, batch, singleMetroDraft)

	response, body := runBatchParse(t, server, "lunch 450 on upi", true)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if body["title"] != "Lunch" || body["items"] != nil || body["kind"] == "batch" {
		t.Fatalf("a batch of one should be today's single draft: %s", response.Body.String())
	}
	if _, has := body["time_slot"]; has {
		t.Fatalf("single draft carries a batch-only key: %s", response.Body.String())
	}
}

func TestParseBatchNothingUnderstoodIsFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server, _ := newBatchTestServer(t, `{"stage":"batch","items":[{},{}]}`, singleMetroDraft)

	response, body := runBatchParse(t, server, "so today I went to the um", true)

	if response.Code != 422 || body["error"] != "non_transactional_prompt" {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if body["credits_charged"] != float64(0) {
		t.Fatalf("credits_charged = %v, want 0", body["credits_charged"])
	}
}

func TestParseBatchCapsAtTenAndReturnsTheRest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	parts := []string{}
	for i := 1; i <= 12; i++ {
		parts = append(parts, batchItem(fmt.Sprintf("Item%d", i), 10*i, "Cash", "evening", i > 1))
	}
	batch := `{"stage":"batch","items":[` + strings.Join(parts, ",") + `]}`
	server, _ := newBatchTestServer(t, batch, singleMetroDraft)

	response, body := runBatchParse(t, server, "a long day", true)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	items, _ := body["items"].([]any)
	if len(items) != ai.MaxBatchItems {
		t.Fatalf("items = %d, want %d", len(items), ai.MaxBatchItems)
	}
	if body["over_cap_text"] != "item11 110, item12 120" {
		t.Fatalf("over_cap_text = %v", body["over_cap_text"])
	}
}

func TestParseBatchDropsUnreadableItemsAndCountsThem(t *testing.T) {
	gin.SetMode(gin.TestMode)
	batch := `{"stage":"batch","items":[` +
		batchItem("Breakfast", 180, "UPI", "morning", false) + `,{},` +
		batchItem("Dinner", 380, "UPI", "night", true) + `]}`
	server, _ := newBatchTestServer(t, batch, singleMetroDraft)

	response, body := runBatchParse(t, server, "breakfast 180, something, dinner 380", true)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 || body["dropped"] != float64(1) {
		t.Fatalf("items = %d dropped = %v: %s", len(items), body["dropped"], response.Body.String())
	}
}

// mp4WithDuration builds the smallest file mp4DurationSeconds reads: an
// `ftyp` box, an `mdat` box, then `moov` holding a version-0 `mvhd`.
func mp4WithDuration(seconds int, mdatBytes int) []byte {
	box := func(kind string, body []byte) []byte {
		out := make([]byte, 8, 8+len(body))
		binary.BigEndian.PutUint32(out, uint32(8+len(body)))
		copy(out[4:], kind)
		return append(out, body...)
	}
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[12:16], 1000)
	binary.BigEndian.PutUint32(mvhd[16:20], uint32(seconds*1000))
	file := box("ftyp", []byte("M4A \x00\x00\x00\x00"))
	file = append(file, box("mdat", make([]byte, mdatBytes))...)
	return append(file, box("moov", box("mvhd", mvhd))...)
}

func TestVoiceIsPricedByLength(t *testing.T) {
	cases := []struct {
		name    string
		audio   []byte
		action  ai.ActionCode
		tooLong bool
	}{
		{"20 s small file", mp4WithDuration(20, 80*1024), ai.ActionTransactionParseVoiceShort, false},
		{"90 s speech-quality day", mp4WithDuration(90, 360*1024), ai.ActionTransactionParseVoiceMedium, false},
		{"31 s under 512 KB is still long", mp4WithDuration(31, 120*1024), ai.ActionTransactionParseVoiceMedium, false},
		{"over two minutes", mp4WithDuration(150, 600*1024), "", true},
		{"unreadable small file falls back to size", make([]byte, 100*1024), ai.ActionTransactionParseVoiceShort, false},
		{"unreadable mid file falls back to size", make([]byte, 900*1024), ai.ActionTransactionParseVoiceMedium, false},
		{"over the byte cap", make([]byte, 1600*1024), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, tooLong := classifyVoiceParseAction(tc.audio)
			if action != tc.action || tooLong != tc.tooLong {
				t.Fatalf("got (%q, %v), want (%q, %v)", action, tooLong, tc.action, tc.tooLong)
			}
		})
	}
}

func TestMP4DurationReadsVersion1AndRejectsJunk(t *testing.T) {
	mvhd := make([]byte, 112)
	mvhd[0] = 1
	binary.BigEndian.PutUint32(mvhd[20:24], 44100)
	binary.BigEndian.PutUint64(mvhd[24:32], 44100*42)
	box := func(kind string, body []byte) []byte {
		out := make([]byte, 8, 8+len(body))
		binary.BigEndian.PutUint32(out, uint32(8+len(body)))
		copy(out[4:], kind)
		return append(out, body...)
	}
	seconds, ok := mp4DurationSeconds(box("moov", box("mvhd", mvhd)))
	if !ok || seconds != 42 {
		t.Fatalf("v1 duration = %v, %v", seconds, ok)
	}
	if _, ok := mp4DurationSeconds([]byte{0, 0, 0, 200, 'm', 'o', 'o', 'v'}); ok {
		t.Fatal("a box that runs past the end must not parse")
	}
}

// The 26 Nov device test, as the model answered it: three lean items, none
// with a date, though the text opens with "Yesterday".
func TestParseBatchPutsASpokenDayOnUndatedItems(t *testing.T) {
	gin.SetMode(gin.TestMode)
	text := "Yesterday I had sweets in Haldiram for Rs. 175.99 paid by UPI and then had a lunch in office canteen. Aditya paid for it, Rs. 140"
	batch := `{"stage":"batch","intent":"capture","items":[` +
		`{"type":"expense","title":"Sweets","amount":175.99,"mode":"UPI","category":"Food & Drinks","time_slot":null,"mode_carried":false,"source_text":"I had sweets in Haldiram for Rs. 175.99 paid by UPI"},` +
		`{"type":"expense","title":"Lunch","amount":140,"mode":"UPI","category":"Food & Drinks","time_slot":"afternoon","mode_carried":false,"source_text":"had a lunch in office canteen. Aditya paid for it, Rs. 140"}]}`
	server, _ := newBatchTestServer(t, batch, singleMetroDraft)

	response, body := runBatchParse(t, server, text, true)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	yesterday := todayIn("Asia/Kolkata")
	parsed, _ := time.Parse("2006-01-02", yesterday)
	yesterday = parsed.AddDate(0, 0, -1).Format("2006-01-02")
	for _, value := range body["items"].([]any) {
		item := value.(map[string]any)
		if item["date"] != yesterday {
			t.Fatalf("%v dated %v, want yesterday %s", item["title"], item["date"], yesterday)
		}
		if flags, _ := item["needs_confirmation"].(map[string]any); flags["date"] != nil {
			t.Fatalf("%v still asks to check its date: %v", item["title"], flags)
		}
		for _, field := range item["missing_fields"].([]any) {
			if field == "date" {
				t.Fatalf("%v still lists date as missing", item["title"])
			}
		}
	}
}

func TestFillSpokenDates(t *testing.T) {
	item := func(source string, date any) map[string]any {
		return map[string]any{
			"source_text":        source,
			"date":               date,
			"needs_confirmation": map[string]any{"date": true, "mode": true},
			"missing_fields":     []string{"date"},
		}
	}
	const today = "2026-11-27"

	t.Run("a day word applies forward, not back", func(t *testing.T) {
		items := []map[string]any{
			item("chai 20", nil),
			item("yesterday auto 60", nil),
			item("lunch 140", nil),
			item("today movie 300", nil),
			item("popcorn 200", nil),
		}
		fillSpokenDates(items, "chai 20, yesterday auto 60, lunch 140, today movie 300, popcorn 200", today)
		want := []any{nil, "2026-11-26", "2026-11-26", "2026-11-27", "2026-11-27"}
		for i, it := range items {
			if it["date"] != want[i] {
				t.Fatalf("item %d date = %v, want %v", i, it["date"], want[i])
			}
		}
		// Only the date flag goes; the first item, still undated, keeps it.
		if items[0]["needs_confirmation"].(map[string]any)["date"] != true {
			t.Fatal("an undated item lost its date flag")
		}
		if flags := items[1]["needs_confirmation"].(map[string]any); flags["date"] != nil || flags["mode"] != true {
			t.Fatalf("flags = %v, want only date cleared", flags)
		}
		if missing := items[1]["missing_fields"].([]string); len(missing) != 0 {
			t.Fatalf("missing_fields = %v", missing)
		}
	})

	t.Run("a date the model gave stands and carries", func(t *testing.T) {
		items := []map[string]any{item("auto 60", "2026-11-23"), item("yesterday lunch 140", nil), item("tea 20", nil)}
		items[0]["needs_confirmation"] = map[string]any{}
		fillSpokenDates(items, "on Sunday auto 60, yesterday lunch 140, tea 20", today)
		if items[0]["date"] != "2026-11-23" || items[1]["date"] != "2026-11-26" || items[2]["date"] != "2026-11-26" {
			t.Fatalf("dates = %v %v %v", items[0]["date"], items[1]["date"], items[2]["date"])
		}
	})

	t.Run("day before yesterday, and words that only look like it", func(t *testing.T) {
		items := []map[string]any{item("dinner 400", nil), item("kal auto 50", nil)}
		fillSpokenDates(items, "day before yesterday dinner 400, kal auto 50", today)
		if items[0]["date"] != "2026-11-25" || items[1]["date"] != "2026-11-25" {
			t.Fatalf("dates = %v %v", items[0]["date"], items[1]["date"])
		}
		paidBack := []map[string]any{item("Priya paid me back 200 for yesterday's dinner", nil), item("chai 30", nil)}
		fillSpokenDates(paidBack, "petrol 500, then Priya paid me back 200 for yesterday's dinner, then chai 30", today)
		if paidBack[0]["date"] != nil || paidBack[1]["date"] != nil {
			t.Fatalf("yesterday's dinner dated today's items: %v %v", paidBack[0]["date"], paidBack[1]["date"])
		}
		todays := []map[string]any{item("bread 40", nil)}
		fillSpokenDates(todays, "yesterdays bread 40", today)
		if todays[0]["date"] != nil {
			t.Fatalf("a word containing yesterday dated the item: %v", todays[0]["date"])
		}
	})
}
