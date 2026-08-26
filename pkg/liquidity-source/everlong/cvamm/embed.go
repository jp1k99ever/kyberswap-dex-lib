package everlongcvamm

import _ "embed"

//go:embed abi/CvammALM.json
var almABIJson []byte

//go:embed abi/FeeHook.json
var feeHookABIJson []byte
