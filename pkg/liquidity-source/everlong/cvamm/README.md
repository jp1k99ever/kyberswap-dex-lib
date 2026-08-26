# Everlong CVAMM rollout

This integration is bound to a versioned listing profile. `StaticExtra` records the ALM,
ordered token pair, configured execution fields, implementation identity and profile
hash. The tracker validates that profile before issuing RPC calls, and the simulator
validates it again on every quote and coupled liquidity transition.

## Cache migration

Profile version 1 changes both the persisted `entity.Pool.StaticExtra` payload and the
`PoolSimulator` structure encoded by msgpack with `SetForceAsArray(true)`. Deploy the
pool-service and router-service binaries together. Before enabling routing, discard all
cached `everlong-cvamm` entities and serialized simulators, reset this source's listing
metadata, then run the lister and tracker to completion. Do not decode pre-version-1
simulators with this binary: legacy entries fail closed with `ErrInvalidProfile` and must
be relisted rather than patched in place.
