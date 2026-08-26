package everlongpsm

import (
	"bytes"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

var (
	psmABI       abi.ABI
	debtTokenABI abi.ABI
)

func init() {
	for _, e := range []struct {
		target *abi.ABI
		data   []byte
	}{
		{&psmABI, psmABIJson},
		{&debtTokenABI, debtTokenABIJson},
	} {
		parsed, err := abi.JSON(bytes.NewReader(e.data))
		if err != nil {
			panic(err)
		}
		*e.target = parsed
	}
}
