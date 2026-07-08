// Package modelcache provides an in-memory, thread-safe cache of the model IDs
// that Kiro currently exposes via ListAvailableModels. It implements the
// github.com/RobinGasi/kiro-proxy/internal/model.Cache interface so the model
// resolver can verify whether a (normalized) model name is known.
//
// The cache holds a TTL and a last-refresh timestamp. It never performs any HTTP
// itself: callers inject a fetch function into Refresh. On a failed (or empty)
// refresh the previous contents are retained, so the cache always degrades to
// the last good list — or the seeded fallback list — rather than going blank.
package modelcache

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/model"
)

// Compile-time guarantee that *ModelInfoCache satisfies the resolver's Cache.
var _ model.Cache = (*ModelInfoCache)(nil)

// ModelInfoCache is a thread-safe cache of available model IDs.
//
// It is seeded with a fallback list at construction time and kept fresh by
// periodic calls to Refresh. The zero value is not usable; construct one with
// New.
type ModelInfoCache struct {
	mu         sync.RWMutex
	models     []string            // ordered, de-duplicated list of model IDs
	valid      map[string]struct{} // set view of models for O(1) lookup
	lastUpdate time.Time           // zero until the first successful Refresh
	ttl        time.Duration
	logger     *slog.Logger
}

// New returns a cache seeded with fallback (e.g. IDs derived from
// config.FallbackModels) and a TTL of ttl (typically
// time.Duration(cfg.ModelCacheTTL) * time.Second). Duplicate and empty IDs in
// fallback are dropped, preserving first-seen order.
//
// The last-refresh timestamp is left unset, so NeedsRefresh reports true until
// the first successful Refresh — the fallback contents are usable in the
// meantime. A nil logger falls back to slog.Default().
func New(fallback []string, ttl time.Duration, logger *slog.Logger) *ModelInfoCache {
	if logger == nil {
		logger = slog.Default()
	}
	deduped := dedupe(fallback)
	valid := make(map[string]struct{}, len(deduped))
	for _, id := range deduped {
		valid[id] = struct{}{}
	}
	return &ModelInfoCache{
		models: deduped,
		valid:  valid,
		ttl:    ttl,
		logger: logger,
	}
}

// Refresh calls fetch and atomically replaces the cache with its result on
// success. fetch is the only place I/O happens (ListAvailableModels lives in the
// caller); the cache lock is not held while fetch runs.
//
// On a fetch error the previous (or fallback) contents are kept and the error is
// returned. A successful fetch that yields no usable IDs is also treated as a
// no-op (previous contents kept) so the cache never goes empty. Only a
// successful, non-empty refresh advances the last-refresh timestamp.
func (c *ModelInfoCache) Refresh(ctx context.Context, fetch func(context.Context) ([]string, error)) error {
	if fetch == nil {
		return errors.New("modelcache: fetch function is nil")
	}
	ids, err := fetch(ctx)
	if err != nil {
		// Return the error and let the caller log the outcome once — the existing
		// (or fallback) contents are kept either way.
		return err
	}
	deduped := dedupe(ids)
	if len(deduped) == 0 {
		c.logger.Debug("model cache refresh returned no models; keeping existing contents")
		return nil
	}

	valid := make(map[string]struct{}, len(deduped))
	for _, id := range deduped {
		valid[id] = struct{}{}
	}

	c.mu.Lock()
	c.models = deduped
	c.valid = valid
	c.lastUpdate = time.Now()
	c.mu.Unlock()

	c.logger.Info("model cache refreshed", "count", len(deduped))
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

// dedupe returns ids with empty strings and duplicates removed, preserving the
// order of first appearance.
func dedupe(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
