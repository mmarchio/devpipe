package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// WorkerFactory creates workers from configuration (WBS 12.2). It returns
// the interface, never a concrete adapter.
type WorkerFactory interface {
	Create(ctx context.Context, config WorkerConfig) (Worker, error)
}

// Constructor builds one provider's worker. It must validate its
// provider-specific configuration before any connection attempt.
type Constructor func(ctx context.Context, config WorkerConfig) (Worker, error)

// ErrUnknownProvider is returned for an unregistered provider.
var ErrUnknownProvider = errors.New("worker: unknown provider")

// ProviderRegistry is the WorkerFactory resolving providers to
// constructors.
type ProviderRegistry struct {
	ctors map[WorkerProvider]Constructor
}

var _ WorkerFactory = (*ProviderRegistry)(nil)

// NewProviderRegistry returns an empty registry.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{ctors: map[WorkerProvider]Constructor{}}
}

// Register binds a provider to its constructor.
func (r *ProviderRegistry) Register(p WorkerProvider, c Constructor) {
	r.ctors[p] = c
}

// Providers lists the registered providers (sorted).
func (r *ProviderRegistry) Providers() []WorkerProvider {
	out := make([]WorkerProvider, 0, len(r.ctors))
	for p := range r.ctors {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Create validates the generic configuration, resolves the provider and
// delegates to its constructor (no connection is made for an invalid
// configuration).
func (r *ProviderRegistry) Create(ctx context.Context, config WorkerConfig) (Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	c, ok := r.ctors[config.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, config.Provider)
	}
	return c(ctx, config)
}
