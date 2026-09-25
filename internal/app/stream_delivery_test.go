package app

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func streamProbePatchLine(ops ...map[string]any) string {
	items := make([]any, 0, len(ops))
	for _, op := range ops {
		items = append(items, op)
	}
	raw, _ := json.Marshal(map[string]any{"type": "patch", "v": items})
	return string(raw)
}

func streamProbeAgentStepLine(text string) string {
	return streamProbePatchLine(map[string]any{
		"o": "a", "p": "/s/-",
		"v": map[string]any{
			"id": "step-1", "type": "agent-inference",
			"value": []any{map[string]any{"type": "text", "content": text}},
		},
	})
}

func runStreamProbe(lines []string) ([]string, string) {
	var emitted []string
	sink := InferenceStreamSink{Text: func(delta string) error {
		emitted = append(emitted, delta)
		return nil
	}}
	state := &ndjsonTranscriptState{ActiveAgentIndex: -1}
	scanner := newNDJSONScanner(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	for scanner.Scan() {
		if err := state.handleLine(scanner.Bytes(), "thread-probe", sink); err != nil {
			return emitted, ""
		}
		if state.hasTerminalAnswer() {
			break
		}
	}
	return emitted, state.FinalAgent.Text
}

// A cumulative content patch per character must reach the client as one delta
// per patch, not as a single frame at the end.
func TestCumulativeContentPatchStreamsIncrementally(t *testing.T) {
	answer := "Hello world, streaming works."
	lines := []string{streamProbeAgentStepLine(string(answer[0]))}
	for i := 2; i <= len(answer); i++ {
		lines = append(lines, streamProbePatchLine(map[string]any{
			"o": "a", "p": "/s/0/value/0/content", "v": answer[:i],
		}))
	}
	lines = append(lines, streamProbePatchLine(map[string]any{"o": "a", "p": "/s/0/finishedAt", "v": 1}))

	emitted, finalText := runStreamProbe(lines)
	// Growth that only adds trailing whitespace is legitimately coalesced by the
	// sanitizer, so allow some slack while still proving the stream is incremental
	// rather than one frame at the end.
	if len(emitted) < len(answer)/2 {
		t.Fatalf("expected an incremental stream, got only %d deltas for %d chars", len(emitted), len(answer))
	}
	if joined := strings.Join(emitted, ""); joined != answer {
		t.Fatalf("streamed text = %q, want %q", joined, answer)
	}
	if finalText != answer {
		t.Fatalf("final text = %q, want %q", finalText, answer)
	}
}

// An append that repeats the current tail is real text. Suffix matching used to
// drop it, which silently ate doubled letters and repeated CJK characters.
func TestIncrementalAppendPatchKeepsRepeatedCharacters(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "doubled latin letter", text: "Hello world."},
		{name: "repeated CJK character", text: "谢谢你的帮助"},
		{name: "repeated punctuation", text: "wait...... done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{streamProbeAgentStepLine(string([]rune(tc.text)[0]))}
			for _, r := range []rune(tc.text)[1:] {
				lines = append(lines, streamProbePatchLine(map[string]any{
					"o": "x", "p": "/s/0/value/0/content", "v": string(r),
				}))
			}
			lines = append(lines, streamProbePatchLine(map[string]any{"o": "a", "p": "/s/0/finishedAt", "v": 1}))

			emitted, finalText := runStreamProbe(lines)
			if joined := strings.Join(emitted, ""); joined != tc.text {
				t.Fatalf("streamed text = %q, want %q", joined, tc.text)
			}
			if finalText != tc.text {
				t.Fatalf("final text = %q, want %q", finalText, tc.text)
			}
		})
	}
}

// "x" means append, so a payload that is not a strict growth of the current
// value is a fragment and must be concatenated even when it repeats it.
func TestAppendFragmentRepeatingWholeValueIsAppended(t *testing.T) {
	lines := []string{
		streamProbeAgentStepLine("Hel"),
		streamProbePatchLine(map[string]any{"o": "x", "p": "/s/0/value/0/content", "v": "Hel"}),
		streamProbePatchLine(map[string]any{"o": "a", "p": "/s/0/finishedAt", "v": 1}),
	}
	emitted, finalText := runStreamProbe(lines)
	if finalText != "HelHel" {
		t.Fatalf("final text = %q, want %q", finalText, "HelHel")
	}
	if joined := strings.Join(emitted, ""); joined != "HelHel" {
		t.Fatalf("streamed text = %q, want %q", joined, "HelHel")
	}
}

// A cumulative payload that grows the current value must replace it, not append
// to it, even though the op is "x".
func TestCumulativeGrowthUnderAppendOpReplacesValue(t *testing.T) {
	lines := []string{
		streamProbeAgentStepLine("Hel"),
		streamProbePatchLine(map[string]any{"o": "x", "p": "/s/0/value/0/content", "v": "Hello"}),
		streamProbePatchLine(map[string]any{"o": "x", "p": "/s/0/value/0/content", "v": "Hello world"}),
		streamProbePatchLine(map[string]any{"o": "a", "p": "/s/0/finishedAt", "v": 1}),
	}
	emitted, finalText := runStreamProbe(lines)
	if finalText != "Hello world" {
		t.Fatalf("final text = %q, want %q", finalText, "Hello world")
	}
	if joined := strings.Join(emitted, ""); joined != "Hello world" {
		t.Fatalf("streamed text = %q, want %q", joined, "Hello world")
	}
}

// blockingStreamReader yields one chunk and then stalls until it is closed,
// standing in for an upstream that goes quiet without finishing its answer.
type blockingStreamReader struct {
	first     []byte
	sent      bool
	closed    chan struct{}
	closeOnce sync.Once
}

func (r *blockingStreamReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.first), nil
	}
	<-r.closed
	return 0, io.EOF
}

func (r *blockingStreamReader) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

// A stream that goes idle after producing text is incomplete. Reporting it as a
// clean stop hides the truncation from the client.
func TestIdleClosedStreamIsMarkedTruncated(t *testing.T) {
	previous := ndjsonIdleAfterAnswerTimeout
	ndjsonIdleAfterAnswerTimeout = 40 * time.Millisecond
	defer func() { ndjsonIdleAfterAnswerTimeout = previous }()

	reader := &blockingStreamReader{
		first:  []byte(streamProbeAgentStepLine("partial answer") + "\n"),
		closed: make(chan struct{}),
	}
	var emitted []string
	result, err := consumeNDJSONStreamWithIdleClose(reader, "thread-probe", InferenceStreamSink{
		Text: func(delta string) error {
			emitted = append(emitted, delta)
			return nil
		},
	}, ndjsonIdleAfterAnswerTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Truncated {
		t.Fatalf("expected Truncated=true for an idle-closed stream")
	}
	if joined := strings.Join(emitted, ""); joined != "partial answer" {
		t.Fatalf("streamed text = %q, want %q", joined, "partial answer")
	}
}

// A stream that finishes normally must not be flagged as truncated.
func TestTerminatedStreamIsNotMarkedTruncated(t *testing.T) {
	previous := ndjsonIdleAfterAnswerTimeout
	ndjsonIdleAfterAnswerTimeout = 40 * time.Millisecond
	defer func() { ndjsonIdleAfterAnswerTimeout = previous }()

	body := strings.Join([]string{
		streamProbeAgentStepLine("complete answer"),
		streamProbePatchLine(map[string]any{"o": "a", "p": "/s/0/finishedAt", "v": 1}),
	}, "\n") + "\n"

	result, err := consumeNDJSONStreamWithIdleClose(io.NopCloser(strings.NewReader(body)), "thread-probe", InferenceStreamSink{}, ndjsonIdleAfterAnswerTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Truncated {
		t.Fatalf("a finished stream must not be marked truncated")
	}
}

// A truncated answer must not be advertised to the client as a clean stop.
func TestTruncatedResultReportsLengthFinishReason(t *testing.T) {
	payload := buildChatCompletion(InferenceResult{Text: "cut off mid", Truncated: true}, "test-model", false)
	choices, _ := payload["choices"].([]map[string]any)
	if len(choices) != 1 {
		t.Fatalf("expected one choice, got %d", len(choices))
	}
	if got := choices[0]["finish_reason"]; got != "length" {
		t.Fatalf("finish_reason = %v, want length", got)
	}

	complete := buildChatCompletion(InferenceResult{Text: "all done"}, "test-model", false)
	completeChoices, _ := complete["choices"].([]map[string]any)
	if got := completeChoices[0]["finish_reason"]; got != "stop" {
		t.Fatalf("finish_reason = %v, want stop", got)
	}
}

// A cached replay still has to look like a stream to the client.
func TestReplayStreamIsChunked(t *testing.T) {
	previousDelay := replayChunkDelay
	replayChunkDelay = 0
	defer func() { replayChunkDelay = previousDelay }()

	answer := strings.Repeat("streaming replay text. ", 6)
	var emitted []string
	result, err := emitReplayStream(PromptRunRequest{
		replayResult: &InferenceResult{Text: answer},
	}, func(delta string) error {
		emitted = append(emitted, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emitted) < 2 {
		t.Fatalf("expected a chunked replay, got %d delta(s)", len(emitted))
	}
	if joined := strings.Join(emitted, ""); joined != answer {
		t.Fatalf("replayed text = %q, want %q", joined, answer)
	}
	if result.Text != answer {
		t.Fatalf("result text = %q, want %q", result.Text, answer)
	}
	for _, chunk := range emitted {
		if chunk == "" {
			t.Fatalf("replay must not emit empty deltas")
		}
	}
}

// Chunking must never split a multi-byte character.
func TestSplitReplayChunksPreservesRunes(t *testing.T) {
	text := "中文流式回复测试内容"
	chunks := splitReplayChunks(text, 3)
	if joined := strings.Join(chunks, ""); joined != text {
		t.Fatalf("joined = %q, want %q", joined, text)
	}
	for _, chunk := range chunks {
		if !strings.Contains(text, chunk) {
			t.Fatalf("chunk %q is not a substring of the original text", chunk)
		}
	}
	if got := len(splitReplayChunks("", 8)); got != 0 {
		t.Fatalf("empty text should produce no chunks, got %d", got)
	}
}

// The recorded patch shape is what tells a progressive upstream apart from one
// that hands over the whole answer at the end.
func TestContentPatchShapeIsRecorded(t *testing.T) {
	answer := "abcdef"
	lines := []string{streamProbeAgentStepLine(string(answer[0]))}
	for _, r := range []rune(answer)[1:] {
		lines = append(lines, streamProbePatchLine(map[string]any{
			"o": "x", "p": "/s/0/value/0/content", "v": string(r),
		}))
	}
	lines = append(lines, streamProbePatchLine(map[string]any{"o": "a", "p": "/s/0/finishedAt", "v": 1}))

	state := &ndjsonTranscriptState{ActiveAgentIndex: -1}
	scanner := newNDJSONScanner(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	for scanner.Scan() {
		if err := state.handleLine(scanner.Bytes(), "thread-probe", InferenceStreamSink{}); err != nil {
			t.Fatalf("handleLine: %v", err)
		}
		if state.hasTerminalAnswer() {
			break
		}
	}
	result := state.result()
	if result.ContentPatchCount != len(answer)-1 {
		t.Fatalf("ContentPatchCount = %d, want %d", result.ContentPatchCount, len(answer)-1)
	}
	if result.LargestContentPatchRunes != 1 {
		t.Fatalf("LargestContentPatchRunes = %d, want 1", result.LargestContentPatchRunes)
	}

	// A single block at the end must be visible as one huge patch.
	single := &ndjsonTranscriptState{ActiveAgentIndex: -1}
	scanner = newNDJSONScanner(strings.NewReader(streamProbeAgentStepLine(answer) + "\n"))
	for scanner.Scan() {
		if err := single.handleLine(scanner.Bytes(), "thread-probe", InferenceStreamSink{}); err != nil {
			t.Fatalf("handleLine: %v", err)
		}
	}
	singleResult := single.result()
	if singleResult.ContentPatchCount != 0 || singleResult.LargestContentPatchRunes != 0 {
		t.Fatalf("a step-append carries no content patch, got count=%d largest=%d",
			singleResult.ContentPatchCount, singleResult.LargestContentPatchRunes)
	}
}
