# Everlong PermissionlessPSM

This source intentionally supports one reviewed production profile. Listing and every
state refresh fail closed unless one pinned block proves all of the following:

- the configured stable is the PSM's sole `listedStables` entry and remains enabled;
- the debt token still reports `PSMBonds(psm) == true`, authorizing both mint and burn;
- the direct PermissionlessPSM runtime hash is
  `0xbd95a90edd774ecbd8822a6258e2643379d1e84b4f23400237281460081b0564`;
- `feeHook` is `0x43cBb9e00E91FcffeF1B510f29Bd45355F846af5`, whose deployed
  runtime hash is
  `0x2890a7c2d3a22efec23d7d87311358f9059daf2bfaa1473522730d7ed38233d2`;
- `capHook` and `yieldHook` are zero; and
- `feeCaller` has the reviewed direct `EverlongPsmAdapter` runtime hash
  `0xbb3b9a67f1812ab434d456acdd02c65fc12621e4ed7dd4f88c20d5b3dc32ac6c`
  at that same block.

The reviewed flat hook is independent of PSM book state, but its scalar entry/exit fees,
caller discounts, and exemptions are mutable. The tracker therefore calls `feeBpFor`
for both directions and for the exact configured execution address on every refresh.
It does not assume 5/5 bp; the live tests assert that those are today's rates for a
known non-exempt contract and separately prove that an exempt contract receives 0/0.

`feeCaller` must be the deployed direct adapter address the PSM sees as `msg.sender`
during settlement. An EOA, zero address, anticipated deployment address, or unrelated
contract makes the quote policy wrong even if it happens to receive the same rate today.
Rollout must therefore set this field only after the production adapter has been
deployed. There is no production address in this library, and a test probe address must
never be copied into venue configuration. A `delegatecall` topology presents the
executor's runtime instead and is deliberately rejected until that executor is reviewed
as a separate profile.

This revision expands `StaticExtra` and the simulator's ForceAsArray msgpack shape. Even
though the source has not previously shipped upstream, rollout must flush/rebuild any
persisted pool entities and simulator caches before enabling it. Legacy or partially
decoded objects fail closed at quote time; they are not migrated in place.

Fee-rate updates remain exact within a route because transactions cannot interleave: a
refresh samples the caller's current flat rates, and sequential `UpdateBalance` calls do
not change them. Hook rotations, extra listed stables, and nonzero cap/yield hooks are
unsupported and invalidate tracking rather than being bounded or approximated. State
overrides are also rejected because the ordinary-chain `eth_getCode` probes cannot be
combined honestly with overridden PSM or hook storage.
