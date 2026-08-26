# Everlong coordinated production rollout

The CVAMM, collateral-rebalancer, and PermissionlessPSM integrations persist
versioned `StaticExtra` profiles and ForceAsArray msgpack simulators. Treat their
pool-service entities, listing cursors, and router-service simulator caches as one
release unit. A binary rollout alone is not a cache migration.

Use this order for every release that changes one of those persisted shapes or
profile identities:

1. Stop routing through all three Everlong sources.
2. Deploy the pool-service and router-service binaries from the same release.
3. Flush every persisted pool entity and serialized simulator for
   `everlong-cvamm`, `everlong-rebalancer`, and `everlong-psm`.
4. Reset the listing cursor/metadata for all three sources. Do not retain a cursor
   while deleting its entities: a cursor may correctly suppress a pool it still
   records as listed.
5. Run all three listers and trackers to completion and verify that fresh,
   block-pinned entities and simulators were built from the expected profiles.
6. Re-enable routing only after those checks pass.

Legacy, partially decoded, detached, or cross-wired profiles are rejected rather
than migrated in place. Do not patch cached JSON or msgpack payloads.
