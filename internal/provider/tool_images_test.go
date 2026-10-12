package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"jarvis/internal/core"

	"charm.land/fantasy"
)

// shot is what a ViewImage-like tool returns for testImage.
var shot = core.ToolResult{Content: "shot.png · 1440×900 PNG", Images: []core.Image{testImage}}

const shotMarker = "[image: PNG · 1440×900 · 256 KB]"

type imageTools struct {
	result core.ToolResult
	calls  int
}

func (t *imageTools) Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{{Name: "ViewImage", Description: "Look at an image", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}}}
}
func (t *imageTools) Execute(context.Context, core.ToolCall) core.ToolResult {
	t.calls++
	return t.result
}

func viewStep(id string) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: "ViewImage", ToolCallInput: `{"path":"shot.png"}`}, {Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls}}
}

// mediaPart is the history part fantasy makes of a tool response with an image.
func mediaPart(id string, result core.ToolResult) fantasy.ToolResultPart {
	response := toolResponse(result.Sendable())
	return fantasy.ToolResultPart{ToolCallID: id, ClientMetadata: response.Metadata, Output: fantasy.ToolResultOutputContentMedia{Data: base64.StdEncoding.EncodeToString(response.Data), MediaType: response.MediaType, Text: response.Content}}
}

func toolTurn(id string, output fantasy.ToolResultPart) []fantasy.Message {
	return []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: id, ToolName: "ViewImage", Input: `{"path":"shot.png"}`}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{output}},
	}
}

// mediaOutputs counts the tool outputs in messages that still carry an image.
func mediaOutputs(messages []fantasy.Message) int {
	n := 0
	for _, message := range messages {
		for _, part := range message.Content {
			if result, ok := part.(fantasy.ToolResultPart); ok && isMedia(result.Output) {
				n++
			}
		}
	}
	return n
}

func TestAgentToolSendsOneImageBesideTheText(t *testing.T) {
	second := core.Image{MediaType: "image/jpeg", Width: 640, Height: 480, Data: []byte("jpeg")}
	for _, tc := range []struct {
		name    string
		result  core.ToolResult
		typ     string
		data    []byte
		content string
	}{
		{"image", shot, "image", testImage.Data, shot.Content},
		{"two images", core.ToolResult{Content: "two", Images: []core.Image{testImage, second}}, "image", testImage.Data, "two\n[image not sent: JPG · 640×480 · 4 B; a tool result carries one image]"},
		{"error", core.ToolResult{Content: "failed", IsError: true, Images: []core.Image{testImage}}, "text", nil, "failed\n[image not sent: PNG · 1440×900 · 256 KB; an error result carries text only]"},
		{"record", core.ToolResult{Content: "replayed", Images: core.WithoutImageData([]core.Image{testImage})}, "text", nil, "replayed\n[image: PNG · 1440×900]"},
		{"text", core.ToolResult{Content: "plain"}, "text", nil, "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ended *core.ToolResult
			tool := &agentTool{definition: (&imageTools{}).Definitions()[0], executor: &imageTools{result: tc.result}, emit: func(e core.Event) {
				if e.Kind == "tool_end" {
					ended = e.Result
				}
			}}
			response, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: "ViewImage", Input: `{"path":"shot.png"}`})
			if err != nil {
				t.Fatal(err)
			}
			if response.Type != tc.typ || !bytes.Equal(response.Data, tc.data) || response.Content != tc.content || response.IsError != tc.result.IsError {
				t.Fatalf("response %s %q (%d bytes)", response.Type, response.Content, len(response.Data))
			}
			// The UI is shown exactly what the model is sent.
			if ended == nil || ended.Content != tc.content || (len(ended.Images) == 1) != (tc.data != nil) {
				t.Fatalf("tool_end %+v", ended)
			}
		})
	}
}

func TestAppendTranscriptNamesToolImagesWithoutTheirBytes(t *testing.T) {
	session := &core.Session{}
	messages := toolTurn("call-1", mediaPart("call-1", shot))
	messages[1].Content = append(messages[1].Content, fantasy.ToolResultPart{ToolCallID: "call-2", Output: fantasy.ToolResultOutputContentText{Text: "listed"}})
	appendTranscript(session, messages)
	if len(session.Messages) != 3 || session.Messages[1].Content != shot.Content+"\n"+shotMarker || session.Messages[2].Content != "listed" {
		t.Fatalf("transcript %+v", session.Messages)
	}
	// Without the client metadata, the media itself still names the image.
	bare := mediaPart("call-3", core.ToolResult{Images: []core.Image{testImage}})
	bare.ClientMetadata = ""
	session.Messages = nil
	appendTranscript(session, toolTurn("call-3", bare))
	if got := session.Messages[1].Content; got != "[image: PNG · 256 KB]" {
		t.Fatalf("bare media transcript %q", got)
	}
}

func TestTokenEstimateCountsToolImagesNotTheirBase64(t *testing.T) {
	big := shot
	big.Images = []core.Image{{MediaType: "image/png", Width: 2000, Height: 2000, Data: make([]byte, 3<<20)}}
	history := toolTurn("call-1", mediaPart("call-1", big))
	if got := estimateTokens(history); got < imageTokens || got > imageTokens+200 {
		t.Fatalf("estimate %d for a tool image", got)
	}
	if got := estimateToolResults(history); got < imageTokens || got > imageTokens+150 {
		t.Fatalf("tool result estimate %d", got)
	}
	if media := history[1].Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentMedia); len(media.Data) < 4<<20 {
		t.Fatal("estimating altered the history")
	}
}

func TestElideToolImagesKeepsTheLatest(t *testing.T) {
	var history []fantasy.Message
	for i := range keptToolImages + 2 {
		id := fmt.Sprintf("call-%d", i)
		history = append(history, toolTurn(id, mediaPart(id, shot))...)
	}
	original, _ := json.Marshal(history)
	elided := elideToolImages(history, keptToolImages, "elided")
	if again, _ := json.Marshal(elideToolImages(elided, keptToolImages, "elided")); !bytes.Equal(again, mustJSON(t, elided)) {
		t.Fatal("eliding twice changed the history again")
	}
	if after, _ := json.Marshal(history); !bytes.Equal(after, original) {
		t.Fatal("eliding altered the original history")
	}
	if mediaOutputs(elided) != keptToolImages {
		t.Fatalf("%d images kept", mediaOutputs(elided))
	}
	// Each turn is a call and its result; the two oldest results lost their images.
	for turn := range 2 {
		result := elided[2*turn+1].Content[0].(fantasy.ToolResultPart)
		text, ok := result.Output.(fantasy.ToolResultOutputContentText)
		if !ok || text.Text != shot.Content+"\n[image elided: PNG · 1440×900 · 256 KB]" || result.ToolCallID != fmt.Sprintf("call-%d", turn) {
			t.Fatalf("oldest result %+v", result)
		}
	}
	if recent := history[:2*keptToolImages]; mediaOutputs(elideToolImages(recent, keptToolImages, "elided")) != keptToolImages {
		t.Fatal("elided images within the limit")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAPIKeepsOnlyRecentToolImagesInHistoryAndState(t *testing.T) {
	steps := [][]fantasy.StreamPart{}
	for i := range keptToolImages + 2 {
		steps = append(steps, viewStep(fmt.Sprintf("call-%d", i)))
	}
	model := &scriptedAPIModel{steps: append(steps, textStep("Seen them all.", fantasy.Usage{}))}
	var states []int
	backend := &API{Model: model, Provider: "anthropic", Checkpoint: func(s *core.Session) error { states = append(states, len(s.State)); return nil }}
	session := &core.Session{}
	if err := backend.Run(context.Background(), session, core.Prompt{Text: "Watch the screen"}, &imageTools{result: shot}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	for i, call := range model.calls {
		if got, want := mediaOutputs(call.Prompt), min(i, keptToolImages); got != want {
			t.Fatalf("step %d sent %d images, want %d", i+1, got, want)
		}
	}
	if got := mediaOutputs(decodeHistory(t, session)); got != keptToolImages {
		t.Fatalf("saved history holds %d images", got)
	}
	// Each image is ~350 KB as base64; the saved state stops growing with them.
	if last := states[len(states)-1]; last > (keptToolImages+1)*400<<10 {
		t.Fatalf("saved state of %d bytes", last)
	}
	for _, message := range session.Messages {
		if message.Role == "tool" && message.Content != shot.Content+"\n"+shotMarker {
			t.Fatalf("transcript tool entry %q", message.Content)
		}
	}
	if data := mustJSON(t, session.Messages); len(data) > 4<<10 {
		t.Fatalf("transcript of %d bytes holds image data", len(data))
	}
}

func TestAPISendsToolImagesToCompatibleModels(t *testing.T) {
	var mu sync.Mutex
	var requests [][]map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, body.Messages)
		n := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			fmt.Fprint(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"ViewImage\",\"arguments\":\"{\\\"path\\\":\\\"shot.png\\\"}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"A login form.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	session := &core.Session{}
	var ended *core.ToolResult
	emit := func(e core.Event) {
		if e.Kind == "tool_end" {
			ended = e.Result
		}
	}
	if err = backend.Run(context.Background(), session, core.Prompt{Text: "What is on screen?"}, &imageTools{result: shot}, emit); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("%d requests", len(requests))
	}
	// Chat completions carry the tool's text in the tool message and its image in
	// a user message right after it.
	messages := requests[1]
	at := -1
	for i, message := range messages {
		if message["role"] == "tool" {
			at = i
		}
	}
	if at < 0 || messages[at]["content"] != shot.Content || at+1 >= len(messages) || messages[at+1]["role"] != "user" {
		t.Fatalf("second request %v", messages)
	}
	parts, _ := messages[at+1]["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["image_url"].(map[string]any)["url"] != testImage.DataURL() {
		t.Fatalf("image message %v", messages[at+1])
	}
	if ended == nil || len(ended.Images) != 1 || !bytes.Equal(ended.Images[0].Data, testImage.Data) {
		t.Fatal("the live tool_end lost the image")
	}
	if got := session.Messages[2]; got.Role != "tool" || got.Content != shot.Content+"\n"+shotMarker {
		t.Fatalf("transcript tool entry %+v", got)
	}
}

// geminiServer is a scripted Gemini endpoint: it asks for ViewImage once, then
// answers, recording each request's contents.
type geminiServer struct {
	mu       sync.Mutex
	contents [][]map[string]any
}

func (g *geminiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var body struct {
		Contents []map[string]any `json:"contents"`
	}
	_ = json.Unmarshal(data, &body)
	g.mu.Lock()
	g.contents = append(g.contents, body.Contents)
	n := len(g.contents)
	g.mu.Unlock()
	part := `{"text":"A login form."}`
	if n == 1 {
		part = `{"functionCall":{"name":"ViewImage","args":{"path":"shot.png"}}}`
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[%s]},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}\n\n", part)
}

func TestGeminiReceivesToolImagesAfterItsFunctionResponses(t *testing.T) {
	api := &geminiServer{}
	server := httptest.NewServer(api)
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "google", Model: "gemini-fixture", BaseURL: server.URL, APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	session := &core.Session{}
	if err = backend.Run(context.Background(), session, core.Prompt{Text: "What is on screen?"}, &imageTools{result: shot}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(api.contents) != 2 {
		t.Fatalf("%d requests", len(api.contents))
	}
	contents := api.contents[1]
	if len(contents) != 4 {
		t.Fatalf("second request contents %v", contents)
	}
	// The call is answered by the next content; the image follows that answer.
	call, answer, image := contents[1]["parts"].([]any), contents[2]["parts"].([]any), contents[3]["parts"].([]any)
	if call[0].(map[string]any)["functionCall"] == nil || len(answer) != 1 {
		t.Fatalf("call %v answer %v", call, answer)
	}
	response, _ := answer[0].(map[string]any)["functionResponse"].(map[string]any)
	result, _ := response["response"].(map[string]any)["result"].(string)
	if response["name"] != "ViewImage" || result != shot.Content+"\n[image: see the next message]" {
		t.Fatalf("function response %v", answer[0])
	}
	if contents[1]["role"] != "model" || contents[2]["role"] != "user" || contents[3]["role"] != "user" {
		t.Fatalf("roles %v, %v, %v", contents[1]["role"], contents[2]["role"], contents[3]["role"])
	}
	if len(image) != 2 || !strings.HasPrefix(image[0].(map[string]any)["text"].(string), "Image returned by the ViewImage call ") {
		t.Fatalf("image content %v", contents[3])
	}
	inline, _ := image[1].(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "image/png" || inline["data"] != base64.StdEncoding.EncodeToString(testImage.Data) {
		t.Fatalf("inline image %.200v", inline)
	}
	// Only the request is rewritten: the saved history keeps the media output.
	if got := mediaOutputs(decodeHistory(t, session)); got != 1 {
		t.Fatalf("saved history holds %d media outputs", got)
	}
}

func TestGeminiToolImagesKeepEveryCallAnswered(t *testing.T) {
	history := []fantasy.Message{
		fantasy.NewUserMessage("Compare"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "a", ToolName: "ViewImage", Input: `{}`},
			fantasy.ToolCallPart{ToolCallID: "b", ToolName: "List", Input: `{}`},
			fantasy.ToolCallPart{ToolCallID: "c", ToolName: "ViewImage", Input: `{}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			mediaPart("a", shot),
			fantasy.ToolResultPart{ToolCallID: "b", Output: fantasy.ToolResultOutputContentText{Text: "listed"}},
			mediaPart("c", core.ToolResult{Images: []core.Image{testImage}}),
		}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Same."}}},
	}
	original := mustJSON(t, history)
	request := geminiToolImages(history)
	if !bytes.Equal(mustJSON(t, history), original) {
		t.Fatal("the rewrite altered the history")
	}
	if len(request) != 5 || mediaOutputs(request) != 0 {
		t.Fatalf("request of %d messages, %d media outputs", len(request), mediaOutputs(request))
	}
	answers := request[2].Content
	for i, want := range []string{shot.Content + "\n[image: see the next message]", "listed", "[image: see the next message]"} {
		result := answers[i].(fantasy.ToolResultPart)
		if text, ok := result.Output.(fantasy.ToolResultOutputContentText); !ok || text.Text != want {
			t.Fatalf("answer %d: %+v", i, result.Output)
		}
	}
	images := request[3]
	if images.Role != fantasy.MessageRoleUser || len(images.Content) != 4 {
		t.Fatalf("image message %+v", images)
	}
	for i, id := range []string{"a", "c"} {
		label, file := images.Content[2*i].(fantasy.TextPart), images.Content[2*i+1].(fantasy.FilePart)
		if label.Text != "Image returned by the ViewImage call "+id+":" || !bytes.Equal(file.Data, testImage.Data) {
			t.Fatalf("image %d: %q", i, label.Text)
		}
	}
	if request[4].Content[0].(fantasy.TextPart).Text != "Same." {
		t.Fatal("later messages moved")
	}
	if other := (&API{Provider: "anthropic"}).request(history); mediaOutputs(other) != 2 {
		t.Fatal("other providers lost their media outputs")
	}
}

func TestCompactionNotesToolImagesWithoutSendingThem(t *testing.T) {
	model := &compactionModel{responses: []*fantasy.Response{completeSummary("Summary.")}}
	history := append([]fantasy.Message{fantasy.NewUserMessage("What is on screen?")}, toolTurn("call-1", mediaPart("call-1", shot))...)
	if _, _, err := (&API{Model: model}).compact(context.Background(), history, "What is on screen?"); err != nil {
		t.Fatal(err)
	}
	request := string(mustJSON(t, model.generateCalls[0].Prompt))
	// The transcript is JSON inside a JSON request, so its newline is escaped twice.
	if !strings.Contains(request, `shot.png · 1440×900 PNG\\n`+shotMarker) || len(request) > 16<<10 {
		t.Fatalf("summarizer request of %d bytes: %.400s", len(request), request)
	}
	if mediaOutputs(history) != 1 {
		t.Fatal("compaction altered the live history")
	}
}

func TestCodexSendsToolImagesAsInputImages(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) {
		// Codex may repeat a call ID; the repeat is answered from the journal.
		for range 2 {
			f.push(map[string]any{"id": "tool", "method": "item/tool/call", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "callId": "call-1", "tool": "ViewImage", "arguments": map[string]any{"path": "shot.png"}}})
		}
		f.finish("completed")
	}
	executor := &imageTools{result: shot}
	session := &core.Session{Directory: t.TempDir()}
	var ended []*core.ToolResult
	if err := (&Codex{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "Look"}, executor, func(e core.Event) {
		if e.Kind == "tool_end" {
			ended = append(ended, e.Result)
		}
	}); err != nil {
		t.Fatal(err)
	}
	var answers [][]map[string]any
	for _, request := range f.requests {
		if _, ok := request["method"]; ok {
			continue
		}
		var result struct {
			Items []map[string]any `json:"contentItems"`
		}
		if err := json.Unmarshal(request["result"], &result); err != nil {
			t.Fatal(err)
		}
		answers = append(answers, result.Items)
	}
	if executor.calls != 1 || len(answers) != 2 || len(ended) != 2 {
		t.Fatalf("%d executions, %d answers, %d tool_end events", executor.calls, len(answers), len(ended))
	}
	first := answers[0]
	if len(first) != 2 || first[0]["type"] != "inputText" || first[0]["text"] != shot.Content || first[1]["type"] != "inputImage" || first[1]["imageUrl"] != testImage.DataURL() {
		t.Fatalf("first answer %.300v", first)
	}
	// A record keeps no bytes, so the replay's marker cannot give the weight.
	if replay := answers[1]; len(replay) != 1 || replay[0]["text"] != shot.Content+"\n[image: PNG · 1440×900]" {
		t.Fatalf("replayed answer %v", replay)
	}
	if len(ended[0].Images) != 1 || len(ended[1].Images) != 0 {
		t.Fatal("tool_end events do not match what was sent")
	}
	journal, err := loadCodexJournal(session.State)
	if err != nil {
		t.Fatal(err)
	}
	recorded := journal.Calls["call-1"].Result
	if recorded == nil || recorded.Content != shot.Content || len(recorded.Images) != 1 || recorded.Images[0].Width != 1440 || recorded.Images[0].Data != nil {
		t.Fatalf("journal entry %+v", recorded)
	}
	if len(session.State) > 4<<10 || len(mustJSON(t, session.Messages)) > 4<<10 {
		t.Fatalf("session holds image data: state %d bytes", len(session.State))
	}
	if got := session.Messages[2]; got.Role != "tool" || got.Content != shot.Content+"\n"+shotMarker {
		t.Fatalf("transcript tool entry %+v", got)
	}
}

func TestCodexContentItemsForImageOnlyResults(t *testing.T) {
	items := codexContentItems(core.ToolResult{Images: []core.Image{testImage}})
	if len(items) != 1 || items[0].(map[string]any)["type"] != "inputImage" {
		t.Fatalf("items %.200v", items)
	}
	if items := codexContentItems(core.ToolResult{}); len(items) != 1 || items[0].(map[string]any)["type"] != "inputText" {
		t.Fatalf("empty result items %v", items)
	}
}

// A model that takes no images rejects every request holding one. Rather
// than fail that way on every follow-up, the session sends its tool images
// as notes from then on and carries on.
func TestAPIContinuesWhenTheModelRejectsImages(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		if strings.Contains(string(body), "image_url") { // As DeepSeek answers.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"Failed to deserialize the JSON body into the target type: messages[3]: unknown variant `+"`image_url`, expected `text`"+`","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			fmt.Fprint(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"ViewImage\",\"arguments\":\"{\\\"path\\\":\\\"shot.png\\\"}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"I cannot see images.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "text-only", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	session := &core.Session{}
	var warnings []string
	emit := func(e core.Event) {
		if e.Kind == "warning" {
			warnings = append(warnings, e.Text)
		}
	}
	if err = backend.Run(context.Background(), session, core.Prompt{Text: "What is on screen?"}, &imageTools{result: shot}, emit); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 || !strings.Contains(bodies[1], "image_url") || strings.Contains(bodies[2], "image_url") || !strings.Contains(bodies[2], `[image not sent, the model rejected it: PNG · 1440×900 · 256 KB]`) {
		t.Fatalf("%d requests; the last:\n%s", len(bodies), bodies[len(bodies)-1])
	}
	if len(warnings) != 1 || mediaOutputs(decodeHistory(t, session)) != 0 {
		t.Fatalf("warnings %q; saved history keeps %d images", warnings, mediaOutputs(decodeHistory(t, session)))
	}
	// A follow-up continues from the saved history, which holds no image.
	if err = backend.Run(context.Background(), session, core.Prompt{Text: "Describe it from the notes"}, &imageTools{result: shot}, emit); err != nil || len(bodies) != 4 {
		t.Fatalf("follow-up: %v after %d requests", err, len(bodies))
	}
}
