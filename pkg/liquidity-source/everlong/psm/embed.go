package everlongpsm

import (
	_ "embed"
)

//go:embed abi/PermissionlessPSM.json
var psmABIJson []byte

//go:embed abi/DebtToken.json
var debtTokenABIJson []byte
