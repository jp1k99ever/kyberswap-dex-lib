package everlongrebalancer

import _ "embed"

//go:embed abi/Rebalancer.json
var rebalancerABIJson []byte

//go:embed abi/Swapper.json
var swapperABIJson []byte

//go:embed abi/ALMAdapter.json
var almABIJson []byte

//go:embed abi/CvammALM.json
var cvammALMABIJson []byte

//go:embed abi/CollVault.json
var collVaultABIJson []byte

//go:embed abi/PositionManager.json
var positionManagerABIJson []byte

//go:embed abi/BorrowerOperations.json
var borrowerOperationsABIJson []byte

//go:embed abi/CollRebalancerMath.json
var mathABIJson []byte

//go:embed abi/Core.json
var coreABIJson []byte

//go:embed abi/DepositAllowlist.json
var depositAllowlistABIJson []byte

//go:embed abi/DebtToken.json
var debtTokenABIJson []byte
