// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"github.com/hashicorp/golang-lru"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type ClientCache interface {
	Get(string) (Client, bool)
	Add(Client) (bool, error)
	Remove(string) bool
}

type ObjectKeyCache interface {
	Add(ctrlclient.ObjectKey, string) bool
	Get(ctrlclient.ObjectKey) (string, bool)
	Remove(ctrlclient.ObjectKey) bool
}

var _ ObjectKeyCache = (*objectKeyCache)(nil)

type objectKeyCache struct {
	// ObjectKey cache mapping a client.ObjectKey to Client cache key.
	// Used for detecting cache key changes between calls to GetClient
	cache *lru.Cache
}

func (o objectKeyCache) Add(key ctrlclient.ObjectKey, cacheKey string) bool {
	return o.cache.Add(key, cacheKey)
}

func (o objectKeyCache) Get(key ctrlclient.ObjectKey) (string, bool) {
	if v, ok := o.cache.Get(key); ok {
		return v.(string), ok
	}

	return "", false
}

func (o objectKeyCache) Remove(key ctrlclient.ObjectKey) bool {
	return o.cache.Remove(key)
}

var _ ClientCache = (*clientCache)(nil)

type clientCache struct {
	cache *lru.Cache
}

func (c *clientCache) Get(key string) (Client, bool) {
	var cacheEntry Client
	raw, ok := c.cache.Get(key)
	if ok {
		cacheEntry = raw.(Client)
	}
	return cacheEntry, ok
}

func (c *clientCache) Add(client Client) (bool, error) {
	cacheKey, err := client.GetCacheKey()
	if err != nil {
		return false, err
	}
	return c.cache.Add(cacheKey, client), nil
}

func (c *clientCache) Remove(key string) bool {
	return c.cache.Remove(key)
}

func NewClientCache(size int) (ClientCache, error) {
	lruCache, err := lru.New(size)
	if err != nil {
		return nil, err
	}

	return &clientCache{cache: lruCache}, nil
}

func NewObjectKeyCache(size int) (ObjectKeyCache, error) {
	lruCache, err := lru.New(size)
	if err != nil {
		return nil, err
	}

	return &objectKeyCache{cache: lruCache}, nil
}