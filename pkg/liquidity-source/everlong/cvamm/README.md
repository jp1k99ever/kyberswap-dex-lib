# Everlong CVAMM rollout

This integration is bound to a versioned listing profile. `StaticExtra` records the ALM,
ordered token pair, configured execution fields, implementation identity and profile
hash. The tracker validates that profile before issuing RPC calls, and the simulator
validates it again on every quote and coupled liquidity transition.

## Cache migration

Profile version 1 changes both the persisted `entity.Pool.StaticExtra` payload and the
`PoolSimulator` structure encoded by msgpack with `SetForceAsArray(true)`. Follow the
[coordinated Everlong rollout](../README.md): stop routing, deploy the pool and router
binaries together, flush entities and simulators for all three sources, reset all three
listing cursors, relist and retrack, and only then re-enable routing. Do not decode
pre-version-1 simulators with this binary: legacy entries fail closed with
`ErrInvalidProfile` and must be relisted rather than patched in place.
