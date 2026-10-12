// Package modelcache provides an in-memory, thread-safe cache of the models that
// Kiro currently exposes via ListAvailableModels, together with the per-model
// token limits that response carries. It implements the
// github.com/RobinGasi/kiro-proxy/internal/model.Cache interface so the model
// resolver can verify whether a (normalized) model name is known and answer
// capability questions from live data.
//
// The cache holds a TTL and a last-refresh timestamp. It never performs any HTTP
// itself: callers inject a fetch function into Refresh. On a failed (or empty)
// refresh the previous contents are retained, so the cache always degrades to
// the last good catalog — or the seeded fallback list — rather than going blank.
package modelcache

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/model"
)

// Compile-time guarantee that *ModelInfoCache satisfies the resolver's Cache.
var _ model.Cache = (*ModelInfoCache)(nil)

// ModelInfoCache is a thread-safe cache of the available models.
//
// It is seeded with a fallback list at construction time and kept fresh by
// periodic calls to Refresh. The zero value is not usable; construct one with
// New.
type ModelInfoCache struct {
	mu      sync.RWMutex
	models  []string
	valid   map[string]struct{}
	caps    map[string]model.Capabilities
	schemas map[string][]byte

	lastUpdate time.Time
	ttl        time.Duration
	logger     *slog.Logger
}

// New returns a cache seeded with fallback (e.g. IDs derived from
// config.FallbackModels) and a TTL of ttl (typically
// time.Duration(cfg.ModelCacheTTL) * time.Second). Duplicate and empty IDs in
// fallback are dropped, preserving first-seen order.
//
// The seed carries IDs only: capability lookups miss until the first successful
// Refresh, which makes the resolver fall back to its own offline table rather
// than inventing limits.
//
// The last-refresh timestamp is left unset, so NeedsRefresh reports true until
// the first successful Refresh — the fallback contents are usable in the
// meantime. A nil logger falls back to slog.Default().
func New(fallback []string, ttl time.Duration, logger *slog.Logger) *ModelInfoCache {
	if logger == nil {
		logger = slog.Default()
	}
	entries := make([]model.CatalogModel, 0, len(fallback))
	for _, id := range fallback {
		entries = append(entries, model.CatalogModel{ID: id})
	}
	models, valid, caps, schemas := index(entries)
	return &ModelInfoCache{
		models:  models,
		valid:   valid,
		caps:    caps,
		schemas: schemas,
		ttl:     ttl,
		logger:  logger,
	}
}

// Refresh calls fetch and atomically replaces the cache with its result on
// success. fetch is the only place I/O happens (ListAvailableModels lives in the
// caller); the cache lock is not held while fetch runs.
//
// On a fetch error the previous (or fallback) contents are kept and the error is
// returned. A successful fetch that yields no usable models is also treated as a
// no-op (previous contents kept) so the cache never goes empty. Only a
// successful, non-empty refresh advances the last-refresh timestamp.
func (c *ModelInfoCache) Refresh(ctx context.Context, fetch func(context.Context) ([]model.CatalogModel, error)) error {
	if fetch == nil {
		return errors.New("modelcache: fetch function is nil")
	}
	entries, err := fetch(ctx)
	if err != nil {
		// Return the error and let the caller log the outcome once — the existing
		// (or fallback) contents are kept either way.
		return err
	}
	models, valid, caps, schemas := index(entries)
	if len(models) == 0 {
		c.logger.Debug("model cache refresh returned no models; keeping existing contents")
		return nil
	}

	c.mu.Lock()
	c.models = models
	c.valid = valid
	c.caps = caps
	c.schemas = schemas
	c.lastUpdate = time.Now()
	c.mu.Unlock()

	c.logger.Info("model cache refreshed", "count", len(models), "with_limits", len(caps))
	return nil
}

// IsValidModel reports whether normalized is present in the cache. It implements
// model.Cache. An empty name is never valid.
func (c *ModelInfoCache) IsValidModel(normalized string) bool {
	if normalized == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.valid[normalized]
	return ok
}

// Capabilities returns the token limits Kiro reported for normalized. It
// implements model.Cache. ok is false when the model is unknown or the catalog
// carried no limits for it, so the caller can fall back.
func (c *ModelInfoCache) Capabilities(normalized string) (model.Capabilities, bool) {
	if normalized == "" {
		return model.Capabilities{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	caps, ok := c.caps[normalized]
	return caps, ok
}

// AdditionalModelRequestFieldsSchema returns a defensive copy of the schema
// advertised by Kiro for normalized.
func (c *ModelInfoCache) AdditionalModelRequestFieldsSchema(normalized string) []byte {
	if normalized == "" {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]byte(nil), c.schemas[normalized]...)
}

// GetAllModelIDs returns a copy of all cached model IDs. It implements
// model.Cache.
func (c *ModelInfoCache) GetAllModelIDs() []string {
	return c.GetAllModels()
}

// GetAllModels returns a copy of all cached model IDs in cache order. The
// returned slice is safe for the caller to retain or mutate.
func (c *ModelInfoCache) GetAllModels() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.models))
	copy(out, c.models)
	return out
}

// RecordSuccessfulModel appends an unknown exact model ID after Kiro has served
// it successfully. It never invents capabilities or request schema. The return
// value reports whether the cache changed.
func (c *ModelInfoCache) RecordSuccessfulModel(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.valid[id]; exists {
		return false
	}
	c.models = append(c.models, id)
	c.valid[id] = struct{}{}
	c.logger.Info("recorded successfully served model", "model", id)
	return true
}

// NeedsRefresh reports whether the cache has never been refreshed or its TTL has
// elapsed since the last successful refresh.
func (c *ModelInfoCache) NeedsRefresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastUpdate.IsZero() {
		return true
	}
	return time.Since(c.lastUpdate) > c.ttl
}

// Size returns the number of model IDs currently cached.
func (c *ModelInfoCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.models)
}

// index builds the cache's three views from a catalog: the ordered ID list (with
// empty IDs and duplicates dropped, first occurrence winning), its set view, and
// the capability map — which only holds entries that actually reported a limit,
// so a miss is distinguishable from a reported zero.
func index(entries []model.CatalogModel) ([]string, map[string]struct{}, map[string]model.Capabilities, map[string][]byte) {
	ids := make([]string, 0, len(entries))
	valid := make(map[string]struct{}, len(entries))
	caps := make(map[string]model.Capabilities, len(entries))
	schemas := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		if _, seen := valid[e.ID]; seen {
			continue
		}
		valid[e.ID] = struct{}{}
		ids = append(ids, e.ID)
		if e.Capabilities.MaxInputTokens > 0 || e.Capabilities.MaxOutputTokens > 0 {
			caps[e.ID] = e.Capabilities
		}
		if len(e.AdditionalModelRequestFieldsSchema) > 0 {
			schemas[e.ID] = append([]byte(nil), e.AdditionalModelRequestFieldsSchema...)
		}
	}
	return ids, valid, caps, schemas
}
