// Package redisprovider implements a nexus provider backed by Redis.
//
// The storage format and write protocol are described in SPEC.md at the
// repository root.
package redisprovider

import "github.com/redis/go-redis/v9"

// Provider reads remote configs from Redis.
//
// The Redis client is owned by the application: Provider never closes it.
type Provider struct {
	client redis.UniversalClient
}

// New returns a Provider that reads from client.
//
// client may be a standalone, Sentinel or Cluster client.
func New(client redis.UniversalClient) *Provider {
	return &Provider{client: client}
}
