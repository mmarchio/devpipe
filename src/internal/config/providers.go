package config

import (
	"maps"
	"sync"
)

// ProviderStore holds the live worker and compression provider
// configuration. It starts from the loaded configuration and is replaced
// when providers are edited in the console, so new requests use the new
// settings without a restart. Readers always get copies.
type ProviderStore struct {
	mu          sync.RWMutex
	workers     map[string]WorkerProviderConfig
	compressors map[string]CompressionProviderConfig
}

// NewProviderStore starts a store from the configuration's providers.
func NewProviderStore(c Config) *ProviderStore {
	return &ProviderStore{workers: maps.Clone(c.WorkerProviders), compressors: maps.Clone(c.CompressionProviders)}
}

// Workers returns a copy of the worker providers by name.
func (s *ProviderStore) Workers() map[string]WorkerProviderConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.workers)
}

// Worker returns one worker provider.
func (s *ProviderStore) Worker(name string) (WorkerProviderConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.workers[name]
	return p, ok
}

// Compressors returns a copy of the compression providers by name.
func (s *ProviderStore) Compressors() map[string]CompressionProviderConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.compressors)
}

// Compressor returns one compression provider.
func (s *ProviderStore) Compressor(name string) (CompressionProviderConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.compressors[name]
	return p, ok
}

// Replace installs a new provider configuration (both kinds at once).
func (s *ProviderStore) Replace(c Config) {
	s.mu.Lock()
	s.workers, s.compressors = maps.Clone(c.WorkerProviders), maps.Clone(c.CompressionProviders)
	s.mu.Unlock()
}
