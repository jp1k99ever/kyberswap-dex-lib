package everlongpsm

import (
	"context"
	"math/big"
	"time"

	"github.com/KyberNetwork/ethrpc"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/goccy/go-json"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	poollist "github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool/list"
)

type PoolsListUpdater struct {
	config       *Config
	ethrpcClient *ethrpc.Client
}

// Metadata binds the emitted pool to its exact static production profile. Old cursors
// had only a `listed` map; they decode with an empty Profile and therefore relist once
// so persisted pools acquire the fee-hook/caller/topology attestation.
type Metadata struct {
	Profile string `json:"profile,omitempty"`
}

var _ = poollist.RegisterFactoryCE(DexType, NewPoolsListUpdater)

func NewPoolsListUpdater(cfg *Config, ethrpcClient *ethrpc.Client) *PoolsListUpdater {
	return &PoolsListUpdater{
		config:       cfg,
		ethrpcClient: ethrpcClient,
	}
}

// GetNewPools lists the sole supported (debtToken, stable) pair only after one pinned
// snapshot proves the exact production topology: the reviewed PsmFlatFeeHook runtime,
// no cap/yield hooks, one listed stable equal to config, a nonzero execution caller and
// a valid sampled fee in both directions. It keeps re-attesting after listing; mutable
// scalar rates do not relist because the tracker refreshes them every block.
func (u *PoolsListUpdater) GetNewPools(ctx context.Context, metadataBytes []byte) ([]entity.Pool, []byte, error) {
	var metadata Metadata
	if len(metadataBytes) > 0 {
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			return nil, metadataBytes, err
		}
	}

	psm, stable, feeCaller, err := configuredAddresses(u.config)
	if err != nil {
		return nil, metadataBytes, err
	}

	var (
		debtToken, metaCore, feeHook, capHook, yieldHook, listedStable gethcommon.Address
		listedStablesLength                                            = new(big.Int)
		wadOffset                                                      uint64
		entryFee, exitFee                                              = new(big.Int), new(big.Int)
	)
	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    psmABI,
		Target: hexutil.Encode(psm[:]),
		Method: psmMethodDebtToken,
	}, []any{&debtToken}).AddCall(&ethrpc.Call{
		ABI:    psmABI,
		Target: hexutil.Encode(psm[:]),
		Method: psmMethodMetaCore,
	}, []any{&metaCore}).AddCall(&ethrpc.Call{
		ABI: psmABI, Target: hexutil.Encode(psm[:]), Method: psmMethodFeeHook,
	}, []any{&feeHook}).AddCall(&ethrpc.Call{
		ABI: psmABI, Target: hexutil.Encode(psm[:]), Method: psmMethodCapHook,
	}, []any{&capHook}).AddCall(&ethrpc.Call{
		ABI: psmABI, Target: hexutil.Encode(psm[:]), Method: psmMethodYieldHook,
	}, []any{&yieldHook}).AddCall(&ethrpc.Call{
		ABI: psmABI, Target: hexutil.Encode(psm[:]), Method: psmMethodListedLength,
	}, []any{&listedStablesLength}).AddCall(&ethrpc.Call{
		ABI: psmABI, Target: hexutil.Encode(psm[:]), Method: psmMethodListedStables,
		Params: []any{big.NewInt(0)},
	}, []any{&listedStable}).AddCall(&ethrpc.Call{
		ABI: psmABI, Target: hexutil.Encode(psm[:]), Method: psmMethodStables,
		Params: []any{stable},
	}, []any{&wadOffset}).AddCall(&ethrpc.Call{
		ABI: psmABI, Target: hexutil.Encode(psm[:]), Method: psmMethodFeeBpFor,
		Params: []any{feeCaller, stable, true},
	}, []any{&entryFee}).AddCall(&ethrpc.Call{
		ABI: psmABI, Target: hexutil.Encode(psm[:]), Method: psmMethodFeeBpFor,
		Params: []any{feeCaller, stable, false},
	}, []any{&exitFee})
	resp, err := req.Aggregate()
	if err != nil {
		return nil, metadataBytes, err
	}
	if resp.BlockNumber == nil {
		return nil, metadataBytes, ErrInvalidSnapshot
	}

	psmCodeHash, hookCodeHash, feeCallerCodeHash, err := probeProfileCode(ctx, u.ethrpcClient,
		psm, feeHook, feeCaller,
		resp.BlockNumber)
	if err != nil {
		return nil, metadataBytes, err
	}
	psmBonded, err := probePSMBond(ctx, u.ethrpcClient, psm, debtToken, resp.BlockNumber)
	if err != nil {
		return nil, metadataBytes, err
	}
	snapshot := &profileSnapshot{
		PSM: psm, DebtToken: debtToken, MetaCore: metaCore, Stable: stable,
		FeeCaller: feeCaller, FeeHook: feeHook, CapHook: capHook, YieldHook: yieldHook,
		ListedStable: listedStable, ListedStablesLength: listedStablesLength,
		WadOffset: wadOffset, EntryFeeBp: entryFee, ExitFeeBp: exitFee,
		PSMCodeHash: psmCodeHash, FeeHookCodeHash: hookCodeHash, FeeCallerCodeHash: feeCallerCodeHash,
		PSMBonded: psmBonded,
	}
	if err := snapshot.validate(); err != nil {
		return nil, metadataBytes, err
	}

	static := staticExtraFromProfile(snapshot, u.config.GasDeposit, u.config.GasRedeem)
	fingerprint := profileFingerprint(static, u.config.DexID, uint64(u.config.ChainID))
	if metadata.Profile == fingerprint {
		return nil, metadataBytes, nil
	}
	staticExtra, err := json.Marshal(static)
	if err != nil {
		return nil, metadataBytes, err
	}

	poolAddress := hexutil.Encode(psm[:]) + "-" + hexutil.Encode(stable[:])
	pools := []entity.Pool{{
		Address:   poolAddress,
		Exchange:  u.config.DexID,
		Type:      DexType,
		Timestamp: time.Now().Unix(),
		Tokens: []*entity.PoolToken{
			{Address: hexutil.Encode(debtToken[:]), Swappable: true},
			{Address: hexutil.Encode(stable[:]), Swappable: true},
		},
		Reserves:    entity.PoolReserves{"0", "0"},
		StaticExtra: string(staticExtra),
		Extra:       "{}",
		BlockNumber: resp.BlockNumber.Uint64(),
	}}

	metadata.Profile = fingerprint
	newMetadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, metadataBytes, err
	}
	return pools, newMetadataBytes, nil
}
