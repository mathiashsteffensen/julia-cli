package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jarvis/internal/core"

	"charm.land/fantasy"
)

var testImage = core.Image{MediaType: "image/png", Data: bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 64<<10), Width: 1440, Height: 900}

func TestAPISendsAttachedImagesAndKeepsThemAcrossTurns(t *testing.T) {
	requests := make(chan []map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"A login form.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	backend, err := NewAPI(context.Background(), APIConfig{Provider: "compatible", Model: "fixture", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	session := &core.Session{}
	if err = backend.Run(context.Background(), session, core.Prompt{Text: "What is [Image #1]?", Images: []core.Image{testImage}}, &testTools{}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if got := session.Messages[0]; got.Content != "What is [Image #1]?" || len(got.Images) != 1 || got.Images[0].Width != 1440 {
		t.Fatalf("transcript lost the image: %+v", got.Content)
	}
	// The second turn restores the conversation from the saved model state.
	var restored core.Session
	data, _ := json.Marshal(session)
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if err = backend.Run(context.Background(), &restored, core.Prompt{Text: "And now?"}, &testTools{}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	want := testImage.DataURL()
	for turn := range 2 {
		messages := <-requests
		found := false
		for _, message := range messages {
			parts, _ := message["content"].([]any)
			for _, part := range parts {
				part, _ := part.(map[string]any)
				if url, _ := part["image_url"].(map[string]any); url != nil && url["url"] == want {
					found = message["role"] == "user"
				}
			}
		}
		if !found {
			t.Fatalf("turn %d did not send the image", turn+1)
		}
	}
}

func TestTokenEstimateCountsImagesNotTheirEncodedSize(t *testing.T) {
	message := fantasy.NewUserMessage("look", fantasy.FilePart{MediaType: "image/png", Data: make([]byte, 4<<20)})
	if got := estimateTokens(message); got < imageTokens || got > imageTokens+100 {
		t.Fatalf("estimate %d for one image", got)
	}
	history := []fantasy.Message{message, fantasy.NewUserMessage("text only")}
	if got := estimateTokens(history); got < imageTokens || got > imageTokens+200 {
		t.Fatalf("estimate %d for history with one image", got)
	}
	if len(history[0].Content[1].(fantasy.FilePart).Data) == 0 {
		t.Fatal("estimating altered the history")
	}
}

func TestCompactionNotesImagesWithoutSendingThem(t *testing.T) {
	model := &compactionModel{responses: []*fantasy.Response{completeSummary("Summary.")}}
	history := []fantasy.Message{fantasy.NewUserMessage("What is this?", fantasy.FilePart{MediaType: "image/png", Data: testImage.Data})}
	if _, _, err := (&API{Model: model}).compact(context.Background(), history, "What is this?"); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(model.generateCalls[0].Prompt)
	if !strings.Contains(string(request), "[attached image: image/png]") || len(request) > 16<<10 {
		t.Fatalf("summarizer request of %d bytes", len(request))
	}
}

func TestCodexSendsImagesAsDataURLs(t *testing.T) {
	f := newCodexFixture(t)
	f.turn = func(f *codexFixture) { f.finish("completed") }
	session := &core.Session{Directory: t.TempDir()}
	if err := (&Codex{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "Fix [Image #1]", Images: []core.Image{testImage}}, &codexTestExecutor{}, nil); err != nil {
		t.Fatal(err)
	}
	var input []map[string]any
	for _, request := range f.requests {
		if string(request["method"]) == `"turn/start"` {
			var params struct {
				Input []map[string]any `json:"input"`
			}
			if err := json.Unmarshal(request["params"], &params); err != nil {
				t.Fatal(err)
			}
			input = params.Input
		}
	}
	if len(input) != 2 || input[0]["text"] != "Fix [Image #1]" || input[1]["type"] != "image" || input[1]["url"] != testImage.DataURL() {
		t.Fatalf("turn input %v", input)
	}
	if len(session.Messages) == 0 || len(session.Messages[0].Images) != 1 {
		t.Fatal("transcript lost the image")
	}
}

func TestCodexMigrationTranscriptOmitsImageData(t *testing.T) {
	messages := []core.Message{{Role: "user", Content: "see", Images: []core.Image{testImage}}}
	data, _ := json.Marshal(withoutImageData(messages))
	if len(data) > 1024 || !strings.Contains(string(data), `"width":1440`) {
		t.Fatalf("migration transcript %s", data)
	}
	if len(messages[0].Images[0].Data) == 0 {
		t.Fatal("stripping altered the session")
	}
}

func TestCompactionRetainsTheTaskImages(t *testing.T) {
	const window = 1000 // compaction threshold 850
	model := &scriptedAPIModel{steps: [][]fantasy.StreamPart{toolStep(fantasy.Usage{InputTokens: 900, OutputTokens: 5}), textStep("Done.", fantasy.Usage{InputTokens: 30, OutputTokens: 5})}, generated: completeSummary("Continue.")}
	prompt := core.Prompt{Text: "Match [Image #1]", Images: []core.Image{testImage}}
	if err := (&API{Model: model, ContextTokens: window}).Run(context.Background(), &core.Session{}, prompt, &testTools{}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if model.generations != 1 || len(model.calls) != 2 {
		t.Fatalf("summaries=%d steps=%d", model.generations, len(model.calls))
	}
	after := model.calls[1].Prompt
	images := 0
	for _, message := range after {
		for _, part := range message.Content {
			if file, ok := part.(fantasy.FilePart); ok && bytes.Equal(file.Data, testImage.Data) {
				images++
			}
		}
	}
	if images != 1 {
		t.Fatalf("compacted step carries %d copies of the task image", images)
	}
}
