package drm

import (
	"context"
	"strings"
	"sync"
)

// KeySource fetches an asset's keys; *Client implements it, tests fake it.
type KeySource interface {
	Keys(ctx context.Context, a Account, assetID string) (*KeySet, error)
}

// Provider caches KeySets per (account, asset). DRMtoday's keys for an
// asset never change, so entries don't expire; Forget drops an
// application's entries when its DRM settings change. Concurrent requests
// for one asset (an ABR group's renditions publishing together) share a
// single DRMtoday round trip.
type Provider struct {
	src KeySource

	mu      sync.Mutex
	cache   map[string]*KeySet
	pending map[string]*call
}

type call struct {
	done chan struct{}
	ks   *KeySet
	err  error
}

// NewProvider wraps src with the cache.
func NewProvider(src KeySource) *Provider {
	return &Provider{src: src, cache: map[string]*KeySet{}, pending: map[string]*call{}}
}

func cacheKey(a Account, assetID string) string {
	return strings.Join([]string{a.feURL(), a.MerchantUUID, a.CPIXConfigID, assetID}, "\x00")
}

// Keys returns the cached KeySet or fetches it. Failures aren't cached.
func (p *Provider) Keys(ctx context.Context, a Account, assetID string) (*KeySet, error) {
	k := cacheKey(a, assetID)
	p.mu.Lock()
	if ks, ok := p.cache[k]; ok {
		p.mu.Unlock()
		return ks, nil
	}
	if c, ok := p.pending[k]; ok {
		p.mu.Unlock()
		select {
		case <-c.done:
			return c.ks, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	p.pending[k] = c
	p.mu.Unlock()

	c.ks, c.err = p.src.Keys(ctx, a, assetID)

	p.mu.Lock()
	delete(p.pending, k)
	if c.err == nil {
		p.cache[k] = c.ks
	}
	p.mu.Unlock()
	close(c.done)
	return c.ks, c.err
}

// Cached returns a KeySet only if it's already in memory (delivery paths
// that must not block on DRMtoday, e.g. rendering a DVR playlist).
func (p *Provider) Cached(a Account, assetID string) (*KeySet, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ks, ok := p.cache[cacheKey(a, assetID)]
	return ks, ok
}

// Forget drops every cached asset of a merchant/CPIX config, after an
// application's DRM settings are edited.
func (p *Provider) Forget(a Account) {
	prefix := strings.Join([]string{a.feURL(), a.MerchantUUID, a.CPIXConfigID}, "\x00") + "\x00"
	p.mu.Lock()
	for k := range p.cache {
		if strings.HasPrefix(k, prefix) {
			delete(p.cache, k)
		}
	}
	p.mu.Unlock()
}

// AssetID expands an asset ID template: {app}, {stream} and {group} (the
// ABR group a stream is a rendition of, else the stream name), so all
// renditions of a group share one asset and one license, with the video
// key picked per resolution by the CPIX usage rules. An empty template
// means "{app}_{group}".
func AssetID(tmpl, app, stream, group string) string {
	if tmpl == "" {
		tmpl = "{app}_{group}"
	}
	if group == "" {
		group = stream
	}
	return strings.NewReplacer("{app}", app, "{stream}", stream, "{group}", group).Replace(tmpl)
}
