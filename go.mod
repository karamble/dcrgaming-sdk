module github.com/karamble/dcrgaming-sdk

go 1.25.0

require (
	github.com/decred/dcrd/chaincfg/chainhash v1.0.5
	github.com/decred/dcrd/chaincfg/v3 v3.2.1
	github.com/decred/dcrd/crypto/blake256 v1.1.0
	github.com/decred/dcrd/dcrec v1.0.1
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0
	// txscript and wire are pinned; the escrow scripts build on these exact
	// versions and a bump changes bytes guarding live mainnet bonds.
	github.com/decred/dcrd/txscript/v4 v4.1.1
	github.com/decred/dcrd/wire v1.7.0
	github.com/decred/slog v1.2.0
	// grpc's own go directive is 1.25.0, equal to this module's; bumping grpc
	// is a toolchain-floor decision for dcrpoker, not a routine update.
	google.golang.org/grpc v1.83.0
	google.golang.org/protobuf v1.36.12
)
