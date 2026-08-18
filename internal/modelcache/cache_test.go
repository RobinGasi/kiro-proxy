package modelcache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"git.gasi.dev/robin/kiro-proxy/internal/model"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSeedHasNoCapabilities pins that the fallback seed carries IDs only: a
// capability lookup must miss so the resolver falls back to its own table rather
// than reading an invented zero limit.
func TestSeedHasNoCapabilities(t *testing.T) {
	c := New([]string{"claude-opus-5", "gpt-5.6-sol"}, time.Minute, quietLogger())

	if !c.IsValidModel("gpt-5.6-sol") {
		t.Error("seeded model should be valid")
	}
	if caps, ok := c.Capabilities("gpt-5.6-sol"); ok {
		t.Errorf("seed reported capabilities %+v; want miss", caps)
	}
	if !c.NeedsRefresh() {
		t.Error("a never-refreshed cache must need a refresh")
	}
}

func TestRefreshStoresCapabilities(t *testing.T) {
	c := New([]string{"claude-opus-5"}, time.Minute, quietLogger())

	err := c.Refresh(context.Background(), func(context.Context) ([]model.CatalogModel, error) {
		return []model.CatalogModel{
			{ID: "gpt-5.6-sol", Capabilities: model.Capabilities{MaxInputTokens: 272_000, MaxOutputTokens: 128_000}},
			// Kiro omitted limits for this entry.
			{ID: "mystery-model"},
			// Empty ids and duplicates are dropped; the first occurrence wins.
			{ID: ""},
			{ID: "gpt-5.6-sol", Capabilities: model.Capabilities{MaxInputTokens: 1}},
		}, nil
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := c.GetAllModels(); len(got) != 2 || got[0] != "gpt-5.6-sol" || got[1] != "mystery-model" {
		t.Errorf("catalog = %v, want [gpt-5.6-sol mystery-model]", got)
	}
	if caps, ok := c.Capabilities("gpt-5.6-sol"); !ok || caps.MaxInputTokens != 272_000 || caps.MaxOutputTokens != 128_000 {
		t.Errorf("capabilities = %+v, %v; want 272000/128000, true", caps, ok)
	}
	// Reported no limits → miss, not a zero-valued hit.
	if caps, ok := c.Capabilities("mystery-model"); ok {
		t.Errorf("capabilities(mystery) = %+v, true; want miss", caps)
	}
	// The previous seed is replaced, not merged.
	if c.IsValidModel("claude-opus-5") {
		t.Error("stale seed model still valid after refresh")
	}
	if c.NeedsRefresh() {
		t.Error("cache should be fresh after a successful refresh")
	}
}

// TestRefreshFailureKeepsCatalog pins the degradation contract: a failed or
// empty refresh must never blank the catalog or its capabilities.
func TestRefreshFailureKeepsCatalog(t *testing.T) {
	c := New(nil, time.Minute, quietLogger())
	good := []model.CatalogModel{
		{ID: "gpt-5.6-sol", Capabilities: model.Capabilities{MaxInputTokens: 272_000, MaxOutputTokens: 128_000}},
	}
	if err := c.Refresh(context.Background(), func(context.Context) ([]model.CatalogModel, error) {
		return good, nil
	}); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	wantErr := errors.New("upstream down")
	if err := c.Refresh(context.Background(), func(context.Context) ([]model.CatalogModel, error) {
		return nil, wantErr
	}); !errors.Is(err, wantErr) {
		t.Errorf("refresh error = %v, want %v", err, wantErr)
	}
	if err := c.Refresh(context.Background(), func(context.Context) ([]model.CatalogModel, error) {
		return nil, nil
	}); err != nil {
		t.Errorf("empty refresh should be a no-op, got %v", err)
	}

	if caps, ok := c.Capabilities("gpt-5.6-sol"); !ok || caps.MaxInputTokens != 272_000 {
		t.Errorf("capabilities lost after failed refresh: %+v, %v", caps, ok)
	}
	if c.Size() != 1 {
		t.Errorf("size = %d, want 1", c.Size())
	}
	if err := c.Refresh(context.Background(), nil); err == nil {
		t.Error("a nil fetch must be rejected")
	}
}
