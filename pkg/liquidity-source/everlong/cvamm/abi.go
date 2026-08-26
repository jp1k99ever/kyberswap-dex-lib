package everlongcvamm

import (
	"bytes"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

var (
	almABI     abi.ABI
	feeHookABI abi.ABI
)

func init() {
	for _, e := range []struct {
		target *abi.ABI
		data   []byte
	}{
		{&almABI, almABIJson},
		{&feeHookABI, feeHookABIJson},
	} {
		parsed, err := abi.JSON(bytes.NewReader(e.data))
		if err != nil {
			panic(err)
		}
		*e.target = parsed
	}
}
