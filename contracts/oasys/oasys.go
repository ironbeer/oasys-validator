package oasys

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

const (
	// Built-in contract prefixes.
	BuiltInContractPrefix1 = "0x0000000000000000" // 8 bytes
	BuiltInContractPrefix2 = "0x5200000000000000" // 8 bytes

	// Address of contracts in genesis.
	EnvironmentAddress  = BuiltInContractPrefix1 + "000000000000000000001000"
	StakeManagerAddress = BuiltInContractPrefix1 + "000000000000000000001001"
	AllowListAddress    = BuiltInContractPrefix1 + "000000000000000000001002"

	// Address of initial wallet in genesis.
	mainnetGenesisWalletAddress = "0xdF3548cD5e355202AE92e766c7361eA4F6687A61"
	testnetGenesisWalletAddress = "0xbf9Ec8a822519C00128f0c7C13f13cafF0501Aea"

	// Address of contracts in `oasys-governance-contract`.
	EVMAccessControl = BuiltInContractPrefix2 + "00000000000000000000003F"
)

var (
	GenesisHash        common.Hash
	defaultGenesisHash = common.Hash{}
)

// StateDB is an interface of state.StateDB.
type StateDB interface {
	GetCode(addr common.Address) []byte
	SetCode(addr common.Address, code []byte)
	SetState(addr common.Address, key common.Hash, value common.Hash)
}

// Deploy oasys built-in contracts.
func Deploy(chainConfig *params.ChainConfig, state StateDB, block uint64) {
	if chainConfig == nil || chainConfig.Oasys == nil || state == nil {
		return
	}

	deploymentMap, ok := deploymentSets[GenesisHash]
	if !ok {
		deploymentMap = deploymentSets[defaultGenesisHash]
	}

	if deploymentSet, ok := deploymentMap[block]; ok {
		for _, deployments := range deploymentSet {
			for _, d := range deployments {
				d.deploy(chainConfig, state, block)
			}
		}
	}
}
