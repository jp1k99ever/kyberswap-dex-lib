package everlongrebalancer

import (
	"bytes"
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
)

const (
	opPush1        = byte(0x60)
	opPush20       = byte(0x73)
	opPush32       = byte(0x7f)
	opGas          = byte(0x5a)
	opDelegateCall = byte(0xf4)
)

// runtimeLinksLibrary recognizes the direct Solidity external-library call sequence in
// the deployed legacy implementation: PUSH20 <relocation>; GAS; DELEGATECALL. Requiring
// adjacency proves that the relocated address is the call target; merely remembering an
// earlier PUSH20 until some later DELEGATECALL would accept an unrelated constant while
// the actual target came from another stack slot. Future implementations expose
// mathLibrary() and do not use this legacy fallback.
func runtimeLinksLibrary(code []byte, library common.Address) bool {
	if library == (common.Address{}) {
		return false
	}
	code = stripSolidityMetadata(code)
	for pc := 0; pc < len(code); {
		op := code[pc]
		pc++
		if op < opPush1 || op > opPush32 {
			continue
		}
		width := int(op-opPush1) + 1
		if pc+width > len(code) {
			return false
		}
		if op == opPush20 && bytes.Equal(code[pc:pc+width], library[:]) &&
			pc+width+1 < len(code) && code[pc+width] == opGas &&
			code[pc+width+1] == opDelegateCall {
			return true
		}
		pc += width
	}
	return false
}

// Solidity appends CBOR metadata and its two-byte big-endian length to runtime code.
// It is not executable, but its opaque hash can contain any byte sequence, so remove a
// structurally valid trailer before inspecting opcodes.
func stripSolidityMetadata(code []byte) []byte {
	if len(code) < 3 {
		return code
	}
	metadataLen := int(binary.BigEndian.Uint16(code[len(code)-2:]))
	start := len(code) - metadataLen - 2
	if start < 0 || start >= len(code)-2 {
		return code
	}
	// A Solidity metadata object is a CBOR map (major type 5).
	if code[start] < 0xa0 || code[start] > 0xbf {
		return code
	}
	return code[:start]
}
