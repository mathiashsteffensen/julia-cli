package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"julia/internal/core"
)

// imageAPI is a scripted OpenAI-compatible endpoint: it asks to view shot.png,
// then answers, keeping each request body.
type imageAPI struct {
	mu     sync.Mutex
	bodies []string
}

func (a *imageAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.bodies = append(a.bodies, string(body))
	n := len(a.bodies)
	a.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	const head = `{"id":"c","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,`
	if n == 1 {
		fmt.Fprintf(w, "data: %s\n\n", head+`"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ViewImage","arguments":"{\"path\":\"shot.png\"}"}}]},"finish_reason":null}]}`)
		fmt.Fprintf(w, "data: %s\n\n", head+`"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":900,"completion_tokens":20,"total_tokens":920}}`)
	} else {
		fmt.Fprintf(w, "data: %s\n\n", head+`"delta":{"role":"assistant","content":"A gradient."},"finish_reason":null}]}`)
		fmt.Fprintf(w, "data: %s\n\n", head+`"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2600,"completion_tokens":5,"total_tokens":2605}}`)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func gradientPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 0xff})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestHeadlessJSONDescribesToolImagesWithoutTheirData(t *testing.T) {
	dir := t.TempDir()
	data := gradientPNG(t, 320, 200)
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	api := &imageAPI{}
	// The scripted config is read-only: viewing an image is a read.
	stdout, _, saved := runHeadlessAgainst(t, api, dir, "--json")
	if len(api.bodies) != 2 || !strings.Contains(api.bodies[1], `"url":"data:image/png;base64,`+encoded+`"`) {
		t.Fatalf("the model was not sent the image (%d requests)", len(api.bodies))
	}
	if strings.Contains(stdout, encoded[:64]) {
		t.Fatal("--json events carry the image's data")
	}
	var ended *core.Event
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		var event core.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("bad event line %q: %v", scanner.Text(), err)
		}
		if event.Kind == "tool_end" {
			ended = &event
			if want := fmt.Sprintf(`"images":[{"media_type":"image/png","width":320,"height":200,"bytes":%d}]`, len(data)); !strings.Contains(scanner.Text(), want) {
				t.Fatalf("tool_end line %s\nwant %s", scanner.Text(), want)
			}
		}
	}
	if ended == nil || ended.Result.Content != "shot.png · 320×200 PNG" || len(ended.Result.Images) != 1 || ended.Result.Images[0].Data != nil {
		t.Fatalf("tool_end %+v", ended)
	}
	// The transcript and the audit log keep a description; only the model's
	// history, which the next turn resends, holds the image itself.
	marker := fmt.Sprintf("shot.png · 320×200 PNG\n[image: PNG · 320×200 · %d KB]", (len(data)+512)>>10)
	if tool := saved.Messages[2]; tool.Role != "tool" || tool.Content != marker {
		t.Fatalf("transcript tool entry %+v", tool)
	}
	for name, record := range map[string]any{"messages": saved.Messages, "audit": saved.Audit} {
		if record, _ := json.Marshal(record); bytes.Contains(record, []byte(encoded[:64])) {
			t.Fatalf("saved %s carry the image's data", name)
		}
	}
	if !strings.Contains(string(saved.State), encoded) {
		t.Fatal("the model history lost the image")
	}
}
