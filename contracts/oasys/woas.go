package oasys

import (
	"math/big"
)

var (
	wrappedOAS = &contract{
		name:    "WOAS",
		address: "0x5200000000000000000000000000000000000001",
		code:    wrappedOASCode,
		fixedStorage: map[string]interface{}{
			// string public name
			"0x00": "Wrapped OAS",
			// string public symbol
			"0x01": "WOAS",
			// uint8 public decimals
			"0x02": big.NewInt(18),
		},
	}
)