package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func historyResponse(t *testing.T, data any) string {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLinearReadSessionPreservesPage(t *testing.T) {
	nodes := make([]map[string]any, 20)
	for i := range nodes {
		nodes[i] = map[string]any{
			"id":        fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			"createdAt": "2026-01-01T00:00:00Z",
			"user":      map[string]any{"id": testSessionID, "name": strings.Repeat("user", 1000)},
			"content":   map[string]any{"__typename": "AgentActivityPromptContent", "body": strings.Repeat("large body", 2000)},
		}
	}
	data := map[string]any{"agentSession": map[string]any{
		"id":            testSessionID,
		"issue":         map[string]any{"identifier": "ENG-1", "title": "Long context", "description": strings.Repeat("issue context", 2000)},
		"sourceComment": map[string]any{"id": "source", "body": strings.Repeat("source context", 1000)},
		"comment":       map[string]any{"id": "root", "body": strings.Repeat("root context", 1000)},
		"activities":    map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "cursor-20"}},
	}}
	e, _ := linearFixture(t, "auto", []linearHandler{{match: "agentSession", reply: linearOK(historyResponse(t, data))}})
	result := invoke(e, "Linear", map[string]any{"action": "read_session", "session_id": testSessionID})
	if result.IsError {
		t.Fatal(result.Content)
	}
	if len(result.Content) > 32*1024 || !strings.Contains(result.Content, `end_cursor="cursor-20"`) {
		t.Fatalf("lost cursor or exceeded response budget: %d bytes: %s", len(result.Content), result.Content)
	}
	if !strings.Contains(result.Content, "activity_id") {
		t.Fatal("missing detail retrieval instructions")
	}
	for _, node := range nodes {
		if !strings.Contains(result.Content, node["id"].(string)) {
			t.Errorf("missing activity %s", node["id"])
		}
	}
}

func TestLinearReadSessionRejectsOversizedPage(t *testing.T) {
	nodes := make([]map[string]any, 21)
	for i := range nodes {
		nodes[i] = map[string]any{"id": fmt.Sprintf("00000000-0000-4000-8000-%012d", i)}
	}
	data := map[string]any{"agentSession": map[string]any{"id": testSessionID, "activities": map[string]any{"nodes": nodes, "pageInfo": map[string]any{}}}}
	e, _ := linearFixture(t, "auto", []linearHandler{{match: "agentSession", reply: linearOK(historyResponse(t, data))}})
	result := invoke(e, "Linear", map[string]any{"action": "read_session", "session_id": testSessionID})
	if !result.IsError {
		t.Fatal("expected an error for more than 20 activities")
	}
}

func TestLinearReadSessionActivityChunks(t *testing.T) {
	prompt := strings.Repeat("Hello 🌍\n", 5000)
	action := strings.Repeat("result 🌍\n", 5000)
	tests := []struct {
		name, contentType, want string
		content                 map[string]any
	}{
		{"prompt", "AgentActivityPromptContent", prompt, map[string]any{"body": prompt}},
		{"action", "AgentActivityActionContent", fmt.Sprintf("action=%q\nparameter=%q\nresult=%q", "run", "some parameter", action), map[string]any{"action": "run", "parameter": "some parameter", "result": action}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.content["__typename"] = tt.contentType
			data := map[string]any{"agentActivity": map[string]any{
				"id": testActivityID, "agentSession": map[string]any{"id": testSessionID}, "content": tt.content,
			}}
			e, calls := linearFixture(t, "auto", []linearHandler{{match: "agentActivity", reply: linearOK(historyResponse(t, data))}})
			var got strings.Builder
			for offset := 0; ; {
				result := invoke(e, "Linear", map[string]any{"action": "read_session", "session_id": testSessionID, "activity_id": testActivityID, "offset": offset})
				if result.IsError {
					t.Fatal(result.Content)
				}
				if len(result.Content) > 32*1024 || !utf8.ValidString(result.Content) {
					t.Fatalf("invalid or oversized response: %d bytes", len(result.Content))
				}
				header, chunk, ok := strings.Cut(result.Content, "\n\n")
				if !ok || len(chunk) > 24*1024 {
					t.Fatal("invalid chunk response")
				}
				got.WriteString(chunk)
				if strings.Contains(header, "complete=true") {
					break
				}
				match := regexp.MustCompile(`next_offset=(\d+)`).FindStringSubmatch(header)
				if len(match) != 2 {
					t.Fatalf("missing next offset: %s", header)
				}
				next, err := strconv.Atoi(match[1])
				if err != nil || next <= offset {
					t.Fatalf("invalid next offset: %q", match[1])
				}
				offset = next
			}
			if got.String() != tt.want {
				t.Fatal("chunks did not reconstruct activity text")
			}
			if len(*calls) < 2 {
				t.Fatal("expected multiple detail requests")
			}
		})
	}
}

func TestLinearReadSessionActivityWrongSession(t *testing.T) {
	data := map[string]any{"agentActivity": map[string]any{"id": testActivityID, "agentSession": map[string]any{"id": "00000000-0000-4000-8000-000000000000"}, "content": map[string]any{"__typename": "AgentActivityPromptContent", "body": "private secret"}}}
	e, _ := linearFixture(t, "auto", []linearHandler{{match: "agentActivity", reply: linearOK(historyResponse(t, data))}})
	result := invoke(e, "Linear", map[string]any{"action": "read_session", "session_id": testSessionID, "activity_id": testActivityID})
	if !result.IsError || strings.Contains(result.Content, "private secret") {
		t.Fatalf("wrong-session activity exposed: %s", result.Content)
	}
}

func TestLinearReadSessionInvalidArguments(t *testing.T) {
	tests := []map[string]any{
		{"session_id": "invalid"},
		{"session_id": testSessionID, "activity_id": "invalid"},
		{"session_id": testSessionID, "offset": 1},
		{"session_id": testSessionID, "activity_id": testActivityID, "offset": -1},
		{"session_id": testSessionID, "activity_id": testActivityID, "cursor": "cursor"},
	}
	for _, args := range tests {
		e, calls := linearFixture(t, "auto", nil)
		args["action"] = "read_session"
		result := invoke(e, "Linear", args)
		if !result.IsError || len(*calls) != 0 {
			t.Errorf("arguments %#v: expected error without API call", args)
		}
	}
}

func TestLinearSessionActivityChunkInvalidOffsets(t *testing.T) {
	for _, tc := range []struct {
		body   string
		offset int
	}{
		{string([]byte{0xff}), 0},
		{"a🌍", -1},
		{"a🌍", 2},
		{"a🌍", 6},
	} {
		if _, _, err := linearSessionActivityChunk(tc.body, tc.offset); err == nil {
			t.Errorf("expected error for body %q offset %d", tc.body, tc.offset)
		}
	}
}
