# Everlong collateral-rebalancer rollout

This integration persists a versioned execution profile in `StaticExtra` and a
ForceAsArray msgpack simulator. The profile binds the configured chain and contract
addresses to the discovered token pair, implementation identities, curve identity,
swapper, share-value adapter, and underlying CVAMM base pool. Tracking and quoting fail
closed when that identity is stale or detached.

Changes to this profile or simulator shape require the
[coordinated Everlong rollout](../README.md): stop routing, deploy the pool and router
binaries together, flush entities and simulators for all three sources, reset all three
listing cursors, relist and retrack, and only then re-enable routing. Legacy entries are
not migrated or patched in place.
