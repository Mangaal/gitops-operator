package main

import (
	"sync"
	"time"
)

type DedupCache struct {
	mu       sync.Mutex
	entries  map[string]time.Time
	cooldown time.Duration
}

func NewDedupCache(cooldown time.Duration) *DedupCache {
	return &DedupCache{
		entries:  make(map[string]time.Time),
		cooldown: cooldown,
	}
}

func (d *DedupCache) TryRecord(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()

	if last, ok := d.entries[key]; ok {
		if now.Sub(last) < d.cooldown {
			return false
		}
	}

	d.entries[key] = now
	return true
}

func (d *DedupCache) Cleanup() {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	for k, v := range d.entries {
		if now.Sub(v) > d.cooldown*2 {
			delete(d.entries, k)
		}
	}
}

func buildDedupKey(appUID, signature string) string {
	return appUID + "|" + signature
}

func buildSignature(appUID, syncStatus, healthStatus, revision string) string {
	return appUID + ":" + syncStatus + ":" + healthStatus + ":" + revision
}
