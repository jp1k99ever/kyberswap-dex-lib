package everlongrebalancer

import (
	"bytes"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

var (
	rebalancerABI         abi.ABI
	swapperABI            abi.ABI
	almABI                abi.ABI
	cvammALMABI           abi.ABI
	collVaultABI          abi.ABI
	positionManagerABI    abi.ABI
	borrowerOperationsABI abi.ABI
	mathABI               abi.ABI
	coreABI               abi.ABI
	depositAllowlistABI   abi.ABI
	debtTokenABI          abi.ABI
)

func init() {
	builder := []struct {
		ABI  *abi.ABI
		data []byte
	}{
		{&rebalancerABI, rebalancerABIJson},
		{&swapperABI, swapperABIJson},
		{&almABI, almABIJson},
		{&cvammALMABI, cvammALMABIJson},
		{&collVaultABI, collVaultABIJson},
		{&positionManagerABI, positionManagerABIJson},
		{&borrowerOperationsABI, borrowerOperationsABIJson},
		{&mathABI, mathABIJson},
		{&coreABI, coreABIJson},
		{&depositAllowlistABI, depositAllowlistABIJson},
		{&debtTokenABI, debtTokenABIJson},
	}

	for _, b := range builder {
		var err error
		*b.ABI, err = abi.JSON(bytes.NewReader(b.data))
		if err != nil {
			panic(err)
		}
	}
}
