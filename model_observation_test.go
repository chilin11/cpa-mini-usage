package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestModelObservationStatus(t *testing.T) {
	for _, tc := range []struct{ model, response, status string }{
		{"gpt-5", "", "unknown"}, {"", "gpt-5", "unknown"},
		{"gpt-5", " GPT-5 ", "same"},
		{"claude-sonnet", "claude-sonnet-20250514", "variant"},
		{"gpt-5", "gpt-5-2026-09-01", "variant"},
		{"vendor/gpt-5(high)", "gpt-5-latest", "variant"},
		{"gemini-flash", "gemini-flash-001", "variant"},
		{"gpt-5", "gpt-5-mini", "different"},
		{"gemini-flash-high", "gemini-flash-exp-a", "different"},
		{"gpt-5", "other-gpt-5", "different"},
		{"gpt-5", "gpt-5-beta", "different"},
		{"gpt-5(custom)", "gpt-5", "different"},
		{"gpt-5", "bad\nname", "unknown"},
	} {
		t.Run(tc.model+"/"+tc.response, func(t *testing.T) {
			ev := Event{Model: tc.model, RequestedModel: "unrelated-client-alias", ResponseModel: tc.response}
			if got := modelObservationStatus(ev); got != tc.status {
				t.Fatalf("status %s want %s", got, tc.status)
			}
		})
	}
	for _, name := range []string{strings.Repeat("x", 513), "model\x00name"} {
		if observedModelName(name) != "" {
			t.Fatal("unsafe model name accepted")
		}
	}
}

func TestCaptureModelObservationAndPricing(t *testing.T) {
	for _, body := range []string{
		`{"Model":"test","Alias":"client-alias","ResponseModel":"other","Provider":"codex","Detail":{"InputTokens":1000,"OutputTokens":100}}`,
		`{"record":{"model":"test","alias":"client-alias","response_model":"other","provider":"codex","detail":{"input_tokens":1000,"output_tokens":100}}}`,
	} {
		e := newEngine()
		e.dir = t.TempDir()
		e.prices["openai/test"] = ModelPrice{Input: 2, Output: 8}
		e.prices["openai/other"] = ModelPrice{Input: 200, Output: 800}
		e.capture([]byte(body))
		s := e.snapshot()
		if len(s.Recent) != 1 {
			t.Fatal("missing event")
		}
		ev := s.Recent[0]
		if ev.Model != "test" || ev.RequestedModel != "client-alias" || ev.ResponseModel != "other" || ev.ModelStatus != "different" {
			t.Fatalf("observation %+v", ev)
		}
		if ev.Cost == nil || math.Abs(*ev.Cost-.0028) > 1e-12 || len(s.Credentials[0].Models) != 1 || s.Credentials[0].Models[0].Model != "test" {
			t.Fatal("diagnostic model changed pricing/grouping")
		}
	}
}

func TestModelObservationPersistenceAndLegacyUnknown(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.load()
	e.capture([]byte(`{"Model":"test","Alias":"alias","ResponseModel":"test-20260901","APIKey":"SECRET","ResponseHeaders":{"Authorization":"SECRET"},"Body":"SECRET"}`))
	body, err := os.ReadFile(filepath.Join(e.dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "SECRET") || strings.Contains(string(body), "model_status") || !strings.Contains(string(body), `"response_model":"test-20260901"`) {
		t.Fatalf("unsafe or derived persistence: %s", body)
	}
	restored := newEngine()
	restored.dir = e.dir
	restored.load()
	ev := restored.snapshot().Recent[0]
	if ev.RequestedModel != "alias" || ev.ResponseModel != "test-20260901" || ev.ModelStatus != "variant" {
		t.Fatalf("restore %+v", ev)
	}
	restored.events = []Event{{At: time.Now().UTC(), Model: "test"}}
	ev = restored.snapshot().Recent[0]
	if ev.ModelStatus != "unknown" || ev.RequestedModel != "" || ev.ResponseModel != "" {
		t.Fatalf("invented legacy observation %+v", ev)
	}
	encoded, _ := json.Marshal(ev)
	if strings.Contains(string(encoded), "response_model") || strings.Contains(string(encoded), "requested_model") {
		t.Fatalf("missing raw fields should stay absent: %s", encoded)
	}
}

func TestInvalidObservationDoesNotDiscardUsage(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.capture([]byte(`{"Model":"test","Alias":true,"ResponseModel":"bad\nname","Detail":{"OutputTokens":3}}`))
	ev := e.snapshot().Recent[0]
	if ev.Output != 3 || ev.Model != "test" || ev.ResponseModel != "" || ev.RequestedModel != "" || ev.ModelStatus != "unknown" {
		t.Fatalf("invalid observation damaged usage %+v", ev)
	}
}
