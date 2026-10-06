package app

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPatchSnapshotPreservesIndicesAndStreamsBeforeCompletion(t *testing.T) {
	lines := []string{
		"{\"type\":\"patch-start\",\"version\":1,\"data\":{\"s\":[{\"id\":\"instruction\",\"type\":\"agent-instruction-state\"}]}}",
		streamProbePatchLine(map[string]any{"o": "a", "p": "/s/-", "v": map[string]any{"id": "user", "type": "user"}}),
		"{\"type\":\"patch-sync\",\"version\":1,\"data\":{\"s\":[{\"id\":\"instruction\",\"type\":\"agent-instruction-state\"},{\"id\":\"user\",\"type\":\"user\"},{\"id\":\"answer\",\"type\":\"agent-inference\",\"value\":[{\"type\":\"thinking\",\"content\":\"Consider the request\"}]}]}}",
		streamProbePatchLine(map[string]any{"o": "a", "p": "/s/2/value/-", "v": map[string]any{"type": "text", "content": ""}}),
		streamProbePatchLine(map[string]any{"o": "x", "p": "/s/2/value/1/content", "v": "Hello"}),
		"{\"type\":\"patch-sync\",\"version\":1,\"data\":{\"s\":[{\"id\":\"instruction\",\"type\":\"agent-instruction-state\"},{\"id\":\"user\",\"type\":\"user\"},{\"id\":\"answer\",\"type\":\"agent-inference\",\"value\":[{\"type\":\"thinking\",\"content\":\"Consider the request\"},{\"type\":\"text\",\"content\":\"Hello\"}]}]}}",
		streamProbePatchLine(map[string]any{"o": "x", "p": "/s/2/value/1/content", "v": " world"}),
	}
	var deltas []string
	state := &ndjsonTranscriptState{ActiveAgentIndex: -1}
	sink := InferenceStreamSink{Text: func(delta string) error { deltas = append(deltas, delta); return nil }}
	for _, line := range lines {
		if err := state.handleLine([]byte(line), "thread", sink); err != nil {
			t.Fatal(err)
		}
	}
	if len(deltas) != 2 || strings.Join(deltas, "") != "Hello world" {
		t.Fatalf("expected 2 deltas before completion, got %d", len(deltas))
	}
	if state.FinalAgent.Completed {
		t.Fatal("unfinished snapshot marked complete")
	}
	if len(state.Steps) != 3 {
		t.Fatalf("snapshot must replace steps, got %d", len(state.Steps))
	}
}

// Optional local-only replay. The HAR is never copied into the repository and
// assertions/logging report counts only, not messages, cookies or identifiers.
func TestCapturedStreamReplay(t *testing.T) {
	path := os.Getenv("N2A_TEST_HAR")
	if path == "" {
		t.Skip("set N2A_TEST_HAR to replay a local capture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read local capture")
	}
	var capture struct {
		Log struct {
			Entries []struct {
				Request struct {
					URL      string
					PostData struct{ Text string }
				}
				Response struct {
					Content struct {
						Text     string
						Encoding string
					}
				}
			}
		}
	}
	if json.Unmarshal(raw, &capture) != nil {
		t.Fatal("invalid capture")
	}
	found := false
	for _, e := range capture.Log.Entries {
		if !strings.HasSuffix(e.Request.URL, "/runInferenceTranscript") {
			continue
		}
		found = true
		body := e.Response.Content.Text
		if e.Response.Content.Encoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(body)
			if err != nil {
				t.Fatal("invalid encoded body")
			}
			body = string(decoded)
		}
		var request struct{ ThreadID string }
		if json.Unmarshal([]byte(e.Request.PostData.Text), &request) != nil {
			t.Fatal("invalid inference request")
		}
		state := &ndjsonTranscriptState{ActiveAgentIndex: -1}
		chunks := 0
		var combined strings.Builder
		sink := InferenceStreamSink{Text: func(delta string) error { chunks++; combined.WriteString(delta); return nil }}
		beforeRecord := 0
		for _, line := range strings.Split(body, "\n") {
			var event struct{ Type string }
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			if event.Type == "record-map" {
				beforeRecord = chunks
			}
			if err := state.handleLine([]byte(line), request.ThreadID, sink); err != nil {
				t.Fatal("captured stream parse failed")
			}
		}
		if beforeRecord < 5 {
			t.Fatalf("only %d text chunks before the final record", beforeRecord)
		}
		if strings.TrimSpace(combined.String()) != strings.TrimSpace(state.FinalAgent.Text) {
			t.Fatal("streamed chunks differ from final answer")
		}
		t.Logf("replayed %d lines, %d text chunks before final record", state.LineCount, beforeRecord)
	}
	if !found {
		t.Fatal("capture contains no inference")
	}
}

func TestPatchSnapshotDoesNotReplayPreviousTurns(t *testing.T) {
	state := &ndjsonTranscriptState{ActiveAgentIndex: -1}
	snapshot := map[string]any{"s": []any{
		map[string]any{"id": "old", "type": "agent-inference", "finishedAt": 1, "value": []any{map[string]any{"type": "text", "content": "previous answer"}}},
		map[string]any{"id": "new-user", "type": "user", "value": []any{}},
	}}
	count := 0
	err := state.applySnapshot(snapshot, InferenceStreamSink{Text: func(string) error { count++; return nil }})
	if err != nil || count != 0 {
		t.Fatal("snapshot replayed a previous turn")
	}
}

func TestPatchSyncRetainsModelEvidence(t *testing.T) {
	state := &ndjsonTranscriptState{ActiveAgentIndex: -1}
	snapshot := map[string]any{"s": []any{map[string]any{"id": "answer", "type": "agent-inference", "model": "reported-runtime"}}}
	if err := state.applySnapshot(snapshot, InferenceStreamSink{}); err != nil {
		t.Fatal(err)
	}
	snapshot["s"] = []any{map[string]any{"id": "answer", "type": "agent-inference", "value": []any{}}}
	if err := state.applySnapshot(snapshot, InferenceStreamSink{}); err != nil {
		t.Fatal(err)
	}
	if len(state.Steps[0].ModelObservations) != 1 || state.Steps[0].ModelObservations[0].Model != "reported-runtime" {
		t.Fatal("sync discarded reported model evidence")
	}
}
