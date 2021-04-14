module github.com/ovrclk/e2e-production

go 1.16

require (
	github.com/avast/retry-go v2.7.0+incompatible
	github.com/cosmos/cosmos-sdk v0.42.4
	github.com/ovrclk/akash v0.12.1
	github.com/pkg/errors v0.9.1
	github.com/rs/zerolog v1.20.0
	github.com/spf13/cobra v1.1.1
	github.com/spf13/pflag v1.0.5
	github.com/spf13/viper v1.7.1
	github.com/tendermint/tendermint v0.34.9
	golang.org/x/sync v0.0.0-20201020160332-67f06af15bc9
)

replace github.com/keybase/go-keychain => github.com/99designs/go-keychain v0.0.0-20191008050251-8e49817e8af4

replace github.com/gogo/protobuf => github.com/regen-network/protobuf v1.3.3-alpha.regen.1

replace google.golang.org/grpc => google.golang.org/grpc v1.33.2

replace github.com/grpc-ecosystem/grpc-gateway => github.com/grpc-ecosystem/grpc-gateway v1.14.7

replace github.com/cosmos/cosmos-sdk => github.com/ovrclk/cosmos-sdk v0.41.4-akash-4

replace github.com/tendermint/tendermint => github.com/ovrclk/tendermint v0.34.9-akash-1

replace (
	github.com/cosmos/ledger-cosmos-go => github.com/ovrclk/ledger-cosmos-go v0.13.2
	github.com/zondax/hid => github.com/troian/hid v0.9.9
	github.com/zondax/ledger-go => github.com/ovrclk/ledger-go v0.13.4
)
