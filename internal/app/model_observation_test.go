package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestThreadRecordFallbackIsBoundedAndPreservesSuccessfulRecords(t *testing.T) {
	for _, scenario := range []string{"complete", "partial", "empty", "unauthorized"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls > 2 {
					t.Error("unbounded fallback")
				}
				if !strings.Contains(r.Header.Get("Referer"), "threadtest") {
					t.Error("thread referer lost")
				}
				if scenario == "unauthorized" {
					w.WriteHeader(401)
					return
				}
				var payload map[string]any
				_ = json.NewDecoder(r.Body).Decode(&payload)
				records := map[string]any{}
				put := func(id string) {
					records[id] = map[string]any{"value": map[string]any{"value": map[string]any{"step": map[string]any{"type": "agent-inference", "model": "test-codename"}}}}
				}
				if calls == 1 {
					if r.URL.Path != "/api/v3/syncRecordValuesSpaceInitial" {
						t.Error("wrong primary endpoint")
					}
					if scenario == "complete" {
						put("one")
						put("two")
					}
					if scenario == "partial" {
						put("one")
					}
				} else {
					if r.URL.Path != "/api/v3/syncRecordValues" {
						t.Error("wrong fallback endpoint")
					}
					if scenario == "partial" && len(sliceValue(payload["requests"])) != 1 {
						t.Error("fallback re-read successful records")
					}
					put("one")
					put("two")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"recordMap": map[string]any{"thread_message": records}})
			}))
			defer server.Close()
			client := newBestEffortTestClient(server.URL)
			data, err := client.syncThreadMessages(t.Context(), "thread-test", []string{"one", "two"})
			if scenario == "unauthorized" {
				if err == nil || calls != 1 {
					t.Fatal("auth failure was retried", err, calls)
				}
				return
			}
			if err != nil || len(mapValue(mapValue(data["recordMap"])["thread_message"])) != 2 {
				t.Fatal("records missing", err)
			}
			want := 2
			if scenario == "complete" {
				want = 1
			}
			if calls != want {
				t.Fatal("unexpected request count", calls)
			}
		})
	}
}

func TestReportedModelsComeFromInferenceEvidence(t *testing.T) {
	step := map[string]any{"type": "agent-inference", "model": "upstream-codename", "value": []any{map[string]any{"type": "thinking", "notionModelName": "upstream-codename", "modelProvider": "test-provider"}, map[string]any{"type": "text", "content": "answer"}}}
	record := map[string]any{"value": map[string]any{"value": map[string]any{"step": step, "data": map[string]any{"completed": true}}}}
	agents := extractAgentMessages(map[string]any{"thread_message": map[string]any{"message": record}})
	observed := agents["message"].ModelObservations
	if len(observed) != 1 || observed[0].Model != "upstream-codename" || observed[0].Provider != "test-provider" || observed[0].Source != "thread_record" {
		t.Fatalf("record evidence lost: %+v", observed)
	}
	message, ok := extractConversationMessageFromThreadRecord("message", record)
	if !ok || len(message.ModelObservations) != 1 {
		t.Fatal("history sync lost model evidence")
	}
	delete(step, "model")
	step["value"] = []any{map[string]any{"type": "text", "content": "answer"}}
	agents = extractAgentMessages(map[string]any{"thread_message": map[string]any{"message": record}})
	if len(agents["message"].ModelObservations) != 0 {
		t.Fatal("missing model was guessed")
	}
}

func TestModelEvidenceFromStreamEventsAndPatches(t *testing.T) {
	for _, stream := range []string{
		`{"type":"agent-inference","id":"one","model":"first","value":[{"type":"thinking","content":"thinking","notionModelName":"first","modelProvider":"provider"}]}
{"type":"agent-inference","id":"two","model":"second","value":[{"type":"text","content":"answer"}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"one","type":"agent-inference","value":[{"type":"thinking","content":"thinking","notionModelName":"first","modelProvider":"provider"}]}}]}
{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"two","type":"agent-inference","value":[{"type":"text","content":"answer"}]}},{"o":"p","p":"/s/1/model","v":"second"}]}`,
	} {
		parsed, err := consumeNDJSONStream(strings.NewReader(stream), "thread", InferenceStreamSink{})
		if err != nil {
			t.Fatal(err)
		}
		models := parsed.FinalAgent.ModelObservations
		if len(models) != 2 || models[0].Model != "first" || models[0].Provider != "provider" || models[1].Model != "second" {
			t.Fatalf("stream evidence lost: %+v", models)
		}
	}
}

func TestModelEvidencePersistsPerTurnWithoutChangingRequestedModel(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := app.State.conversations().Create(ConversationCreateRequest{Model: "requested", Prompt: "first"})
	for _, code := range []string{"first-actual", "second-actual"} {
		if code == "second-actual" {
			if _, err := app.State.conversations().Continue(entry.ID, ConversationCreateRequest{Model: "requested", Prompt: "next"}); err != nil {
				t.Fatal(err)
			}
		}
		app.State.conversations().Complete(entry.ID, InferenceResult{Model: "requested", Text: "answer", ModelSelectionMode: "auto_fallback", ModelObservations: []ModelObservation{{Model: code, Source: "stream"}}})
	}
	entry, _ = app.State.conversations().Get(entry.ID)
	if entry.Model != "requested" || len(entry.Messages) != 4 || entry.Messages[1].ModelObservations[0].Model != "first-actual" || entry.Messages[3].ModelObservations[0].Model != "second-actual" {
		t.Fatal("model evidence overwrote another turn")
	}
	if err := app.State.Store.SaveConversation(entry); err != nil {
		t.Fatal(err)
	}
	loaded, err := app.State.Store.LoadConversations()
	if err != nil || len(loaded) == 0 || loaded[0].Messages[3].RequestedModel != "requested" || loaded[0].Messages[3].ModelSelectionMode != "auto_fallback" {
		t.Fatal("model evidence did not survive persistence", err)
	}
}

func TestAutoContinuationRemovesStaleRequestedModel(t *testing.T) {
	client := newBestEffortTestClient("https://example.test")
	payload, _ := client.buildInferencePayload(PromptRunRequest{Prompt: "next", continuationDraft: &continuationTurnDraft{ConfigID: "config", ContextID: "context", ConfigValue: map[string]any{"model": "stale-codename", "modelFromUser": true}}}, "thread", nil)
	raw, _ := json.Marshal(payload)
	if strings.Contains(string(raw), "stale-codename") || !strings.Contains(string(raw), `"modelFromUser":false`) {
		t.Fatal("Auto continuation retained a forced model")
	}
}

func TestContinuationUpdatedConfigUsesCurrentModel(t *testing.T) {
	for _, model := range []string{"", "current-model"} {
		t.Run("model="+model, func(t *testing.T) {
			var saved map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v3/syncRecordValuesSpaceInitial":
					_ = json.NewEncoder(w).Encode(map[string]any{"recordMap": map[string]any{
						"thread": map[string]any{"thread": map[string]any{"value": map[string]any{"value": map[string]any{"messages": []string{"config", "update"}}}}},
						"thread_message": map[string]any{
							"config": map[string]any{"value": map[string]any{"value": map[string]any{"step": map[string]any{"id": "config", "type": "config", "value": map[string]any{"model": "old-model", "modelFromUser": true}}}}},
							"update": map[string]any{"value": map[string]any{"value": map[string]any{"step": map[string]any{"id": "update", "type": "updated-config", "value": map[string]any{"model": "old-model", "modelFromUser": true, "availableConnectors": []any{"connector"}}}}}},
						},
					}})
				case "/api/v3/saveTransactionsFanout":
					if err := json.NewDecoder(r.Body).Decode(&saved); err != nil {
						t.Error(err)
					}
					_, _ = w.Write([]byte(`{}`))
				default:
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
					http.Error(w, "unexpected endpoint", http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := newBrowserFallbackTestClient(server.URL)
			_, _, _, payload, _, err := client.preparePromptRequest(t.Context(), PromptRunRequest{Prompt: "next", UpstreamThreadID: "thread", PinnedSpaceID: "test-space", NotionModel: model})
			if err != nil {
				t.Fatal(err)
			}
			transactions := sliceValue(saved["transactions"])
			if len(transactions) == 0 {
				t.Fatal("continuation was not saved")
			}
			operations := sliceValue(mapValue(transactions[0])["operations"])
			updated := mapValue(mapValue(mapValue(mapValue(operations[0])["args"])["step"])["value"])
			for _, value := range []map[string]any{updated, transcriptStepValue(t, payload, "config")} {
				if stringValue(value["model"]) != model || booleanValue(value["modelFromUser"]) != (model != "") {
					t.Fatalf("stale selection retained: %+v", value)
				}
				if _, exists := value["model"]; exists && model == "" {
					t.Fatal("Auto must omit the explicit model")
				}
			}
			if len(sliceValue(updated["availableConnectors"])) != 1 {
				t.Fatal("model change discarded connector configuration")
			}
		})
	}
}

func TestRunPromptPreservesStreamModelEvidenceAfterRecordFallback(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, recordModel := range []string{"", "record-model"} {
			name := "buffered/"
			if streaming {
				name = "streaming/"
			}
			t.Run(name+"record="+recordModel, func(t *testing.T) {
				var recordMap map[string]any
				syncCalls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v3/runInferenceTranscript":
						var payload map[string]any
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
						}
						recordMap = buildThreadErrorRecordMap(stringValue(payload["threadId"]), "test-space", "message", "", "", "trace")
						value := unwrapRecordValue(mapValue(recordMap["thread_message"])["message"])
						value["step"] = map[string]any{"id": "message", "type": "agent-inference", "model": recordModel, "value": []any{map[string]any{"type": "text", "content": "answer"}}}
						value["data"] = map[string]any{"completed": true}
						w.Header().Set("Content-Type", "application/x-ndjson")
						_, _ = w.Write([]byte(`{"type":"agent-inference","id":"message","model":"stream-model","value":[{"type":"thinking","content":"thinking"}]}` + "\n"))
					case "/api/v3/syncRecordValuesSpaceInitial":
						syncCalls++
						_ = json.NewEncoder(w).Encode(map[string]any{"recordMap": recordMap})
					default:
						t.Errorf("unexpected endpoint: %s", r.URL.Path)
						http.Error(w, "unexpected endpoint", http.StatusNotFound)
					}
				}))
				defer server.Close()
				client := newBrowserFallbackTestClient(server.URL)
				request := PromptRunRequest{Prompt: "hello", PublicModel: "auto", ModelSelectionMode: "auto"}
				var result InferenceResult
				var err error
				if streaming {
					result, err = client.RunPromptStreamWithSink(t.Context(), request, InferenceStreamSink{})
				} else {
					result, err = client.RunPrompt(t.Context(), request)
				}
				if err != nil || result.Text != "answer" || syncCalls != 2 {
					t.Fatalf("record fallback failed: result=%+v, calls=%d, err=%v", result, syncCalls, err)
				}
				want := 1
				if recordModel != "" {
					want++
				}
				if len(result.ModelObservations) != want || result.ModelObservations[0].Model != "stream-model" {
					t.Fatalf("stream evidence lost: %+v", result.ModelObservations)
				}
				if recordModel != "" && result.ModelObservations[1].Model != recordModel {
					t.Fatalf("record evidence lost: %+v", result.ModelObservations)
				}
			})
		}
	}
}
