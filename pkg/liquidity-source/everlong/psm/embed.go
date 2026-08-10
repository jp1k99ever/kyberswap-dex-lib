package everlongpsm

import (
	_ "embed"
)

//go:embed abi/PermissionlessPSM.json
var psmABIJson []byte

//go:embed abi/FeeHook.json
var feeHookABIJson []byte

//go:embed abi/ERC20.json
var erc20ABIJson []byte
