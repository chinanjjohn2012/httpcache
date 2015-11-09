package lrucache

import (
	"time"

	"github.com/cloudflare/golibs/lrucache"
)

const (
	EXPIRE_TIME uint = 3600
)

type Cache struct {
	cache lrucache.Cache
}

// Get returns the response corresponding to key if present.
func (c *Cache) Get(key string) (resp []byte, ok bool) {
	if value, ok := c.cache.GetNotStale(key); ok {
		if resp, ok := value.([]byte); ok {
			return resp, true
		}
	}

	return nil, false
}

// Set saves a response to the cache as key.
func (c *Cache) Set(key string, resp []byte) {
	c.cache.Set(key, resp, time.Now().Add(time.Duration(EXPIRE_TIME)*time.Second))
}

// Delete removes the response with key from the cache.
func (c *Cache) Delete(key string) {
	c.cache.Del(key)
}

// New returns a new Cache using the provided memcache server(s) with equal
// weight. If a server is listed multiple times, it gets a proportional amount
// of weight.
func New(capacity uint) *Cache {
	return &Cache{
		cache: lrucache.NewMultiLRUCache(4, capacity),
	}
}
