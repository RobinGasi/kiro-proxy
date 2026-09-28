package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"git.gasi.dev/robin/kiro-proxy/internal/account"
	"git.gasi.dev/robin/kiro-proxy/internal/model"
	"time"
)

// TestModelListForCapabilities pins the GET /v1/models contract: a model whose
// limits Kiro reported carries them on the wire, and a model with no known
// limits omits the fields entirely so a client can tell "unknown" from a number.
func TestModelListForCapabilities(t *testing.T) {
	list := ModelListFor([]model.CatalogModel{
		{ID: "gpt-5.6-sol", Capabilities: model.Capabilities{MaxInputTokens: 272_000, MaxOutputTokens: 128_000}},
		{ID: "mystery-model"},
	})

	if list.Object != "list" || len(list.Data) != 2 {
		t.Fatalf("envelope changed: %+v", list)
	}

	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Object string `json:"object"`
		Data   []struct {
			ID              string `json:"id"`
			Object          string `json:"object"`
			OwnedBy         string `json:"owned_by"`
			ContextWindow   *int   `json:"context_window"`
			MaxOutputTokens *int   `json:"max_output_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	sol := decoded.Data[0]
	if sol.ID != "gpt-5.6-sol" || sol.Object != "model" || sol.OwnedBy != "anthropic" {
		t.Errorf("base fields changed: %+v", sol)
	}
	if sol.ContextWindow == nil || *sol.ContextWindow != 272_000 {
		t.Errorf("context_window = %v, want 272000", sol.ContextWindow)
	}
	if sol.MaxOutputTokens == nil || *sol.MaxOutputTokens != 128_000 {
		t.Errorf("max_output_tokens = %v, want 128000", sol.MaxOutputTokens)
	}

	unknown := decoded.Data[1]
	if unknown.ContextWindow != nil || unknown.MaxOutputTokens != nil {
		t.Errorf("unknown model must omit limits, got %+v", unknown)
	}
}

func TestWriteModelProtectionErrorIsNonRetryableUpstream(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeModelProtectionError(recorder, &account.ModelProtectionError{
		ModelID: "claude-opus-5.5",
		Until:   time.Now().Add(time.Hour),
	})

	if recorder.Code != http.StatusLocked {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusLocked)
	}
	retryAfter, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
	if err != nil || retryAfter <= 0 {
		t.Fatalf("Retry-After = %q, want positive seconds", recorder.Header().Get("Retry-After"))
	}
	var response struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Error.Type != "kiro_model_temporarily_restricted" {
		t.Fatalf("error type = %q", response.Error.Type)
	}
	if !strings.Contains(response.Error.Message, "switch models") {
		t.Fatalf("message = %q, want actionable model-switch guidance", response.Error.Message)
	}
}
