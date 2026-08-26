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
	opDelegateCall = byte(0xf4)
	opReturn       = byte(0xf3)
	opRevert       = byte(0xfd)
	opInvalid      = byte(0xfe)
	opSelfDestruct = byte(0xff)
)

// runtimeLinksLibrary recognizes a Solidity external-library relocation in deployed
// runtime bytecode. Link placeholders are 20-byte PUSH immediates; parsing instructions
// (rather than bytes.Contains) prevents an address inside PUSH32 data or compiler metadata
// from masquerading as a link. A later DELEGATECALL is also required, distinguishing an
// ordinary embedded address from an external-library call target.
func runtimeLinksLibrary(code []byte, library common.Address) bool {
	if library == (common.Address{}) {
		return false
	}
	code = stripSolidityMetadata(code)
	linkedPush := false
	for pc := 0; pc < len(code); {
		op := code[pc]
		pc++
		switch op {
		case opDelegateCall:
			if linkedPush {
				return true
			}
		case 0x00, opReturn, opRevert, opInvalid, opSelfDestruct:
			// Do not let a constant in a completed basic block attest an unrelated
			// delegatecall later in the runtime.
			linkedPush = false
		}
		if op < opPush1 || op > opPush32 {
			continue
		}
		width := int(op-opPush1) + 1
		if pc+width > len(code) {
			return false
		}
		if op == opPush20 {
			// The most recently loaded address is the only plausible target. This
			// prevents a different library pushed after a stray matching constant from
			// satisfying the check.
			linkedPush = bytes.Equal(code[pc:pc+width], library[:])
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
