package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

// defaultReclaimFee is what a reclaim pays when the bridge names no fee. These
// spends are one input and one output, so it is generous rather than tight.
const defaultReclaimFee = 10_000

// claim is one thing that can be pulled home: an output, the script it was paid
// into, the key that opens it, and how long its lock is.
type claim struct {
	outpoint  string
	script    []byte
	key       *secp256k1.PrivateKey
	lock      uint32
	sigScript func(script, sig []byte) ([]byte, error)
}

// doReclaim pulls one of this seat's own locked outputs home.
//
// Which output depends on the kind, and each has a different key and a
// different branch of a different script. What they share is everything after
// that, which is why the three kinds meet in one place here rather than being
// written out three times as they are in dcrpoker.
func (r *Runtime) doReclaim(ctx context.Context, req *gamingpb.Reclaim) (string, error) {
	dest := strings.TrimSpace(req.GetDestAddr())
	if dest == "" {
		return "", fmt.Errorf("the bridge named no destination to send it to")
	}
	c, err := r.claimFor(req)
	if err != nil {
		return "", err
	}
	txid, err := r.pullHome(ctx, c, dest, req.GetFeeAtoms())
	if err != nil {
		return "", err
	}
	if req.GetKind() == gamingpb.Reclaim_BOND {
		// The coin is already moving, so failing to write down that it
		// left is worth saying and not worth failing over.
		if err := r.identity.SetBondDeposit(""); err != nil {
			r.log.Errorf("swept the bond as %s but could not forget it: %v", txid, err)
		}
	}
	return txid, nil
}

// claimFor works out which output a reclaim request names.
func (r *Runtime) claimFor(req *gamingpb.Reclaim) (claim, error) {
	switch req.GetKind() {
	case gamingpb.Reclaim_BOND:
		outpoint := r.identity.BondDeposit()
		if outpoint == "" {
			return claim{}, fmt.Errorf("this player holds no bond")
		}
		// No session id, matching the one-per-identity deposit above.
		key, err := r.identity.DeriveKey(r.seatTags.Bond, "")
		if err != nil {
			return claim{}, err
		}
		script, err := escrow.BondScript(key.PubKey().SerializeCompressed(), escrow.MinBondBlocks)
		if err != nil {
			return claim{}, err
		}
		return claim{
			outpoint: outpoint, script: script, key: key,
			lock: escrow.MinBondBlocks, sigScript: escrow.BondSigScript,
		}, nil

	case gamingpb.Reclaim_STAKE, gamingpb.Reclaim_TABLE_BOND:
		// Both name an output a table holds, and a table does not record
		// its deposits until the funding stage exists. Said plainly rather
		// than answered wrongly: a reclaim built against a script this
		// process guessed would be signed and refused by the node, and an
		// operator would read that as their money being stuck.
		return claim{}, fmt.Errorf("reclaiming a %s: %w",
			strings.ToLower(req.GetKind().String()), ErrNotYet)
	}
	return claim{}, fmt.Errorf("this game does not know how to reclaim that")
}

// pullHome checks a claim is really claimable and then spends it.
//
// Every check here is one that costs money if it is skipped, so none of them is
// a formality:
//
//   - Already sweeping. dcrd's gettxout ignores mempool spends even with
//     includemempool set, so an output whose reclaim is already broadcast still
//     reads as coin sitting there. Asking twice gets a double spend. What this
//     process itself did is the one thing it can always answer, and it is the
//     difference between offering a refund once and offering it twice.
//   - Confirmed only. The age of the output is the whole question, so a lookup
//     that counted mempool would answer it wrong.
//   - The script matches the output. BuildTimelockedSpend derives its pkScript
//     from the script it is handed, so it cannot notice being handed the wrong
//     one; dcrd reports that as a stack failure long after the signing.
func (r *Runtime) pullHome(ctx context.Context, c claim, dest string, feeAtoms int64) (string, error) {
	if r.isSweeping(c.outpoint) {
		return "", fmt.Errorf("a spend of %s is already on its way from this process; "+
			"asking again would be a double spend", c.outpoint)
	}
	txid, vout, err := splitOutpoint(c.outpoint)
	if err != nil {
		return "", err
	}
	out, err := r.bridge.Outpoint(ctx, txid, vout)
	if err != nil {
		return "", fmt.Errorf("could not read the output: %w", err)
	}
	switch {
	case !out.Found:
		return "", fmt.Errorf("%s holds no coin - it may already have been spent", c.outpoint)
	case out.Confirmations < int64(c.lock):
		return "", fmt.Errorf("%s has %d confirmations and the lock is %d, so it is not spendable "+
			"for another %d blocks", c.outpoint, out.Confirmations, c.lock,
			int64(c.lock)-out.Confirmations)
	}

	_, want, err := escrow.Address(c.script, r.params)
	if err != nil {
		return "", fmt.Errorf("derive the script's address: %w", err)
	}
	if got := hex.EncodeToString(want); !strings.EqualFold(got, out.PkScriptHex) {
		return "", fmt.Errorf("%s pays %s and this key derives %s, so this is not the script that "+
			"output was paid into; nothing was signed and the coin is untouched",
			c.outpoint, out.PkScriptHex, got)
	}

	payScript, err := payScriptFor(dest, r.params)
	if err != nil {
		return "", err
	}
	if feeAtoms <= 0 {
		feeAtoms = defaultReclaimFee
	}
	prevHash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return "", fmt.Errorf("outpoint txid: %w", err)
	}
	tx, err := escrow.BuildTimelockedSpend(escrow.Spend{
		Key:        c.key,
		Script:     c.script,
		Prevout:    wire.OutPoint{Hash: *prevHash, Index: vout, Tree: wire.TxTreeRegular},
		ValueAtoms: out.ValueAtoms,
		CSVBlocks:  c.lock,
		PayScript:  payScript,
		FeeAtoms:   feeAtoms,
		SigScript:  c.sigScript,
		Params:     r.params,
	})
	if err != nil {
		return "", err
	}
	raw, err := tx.Bytes()
	if err != nil {
		return "", fmt.Errorf("serialise: %w", err)
	}
	sent, err := r.bridge.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		return "", err
	}
	r.noteSweeping(c.outpoint)
	return sent, nil
}

// noteSweeping records that this process broadcast a spend of an outpoint.
func (r *Runtime) noteSweeping(outpoint string) {
	r.sweepMu.Lock()
	defer r.sweepMu.Unlock()
	if r.sweeping == nil {
		r.sweeping = map[string]bool{}
	}
	r.sweeping[outpoint] = true
}

// isSweeping reports whether this process has a spend of the outpoint out.
func (r *Runtime) isSweeping(outpoint string) bool {
	r.sweepMu.Lock()
	defer r.sweepMu.Unlock()
	return r.sweeping[outpoint]
}

// doneSweeping forgets an outpoint the chain has stopped holding, so the map
// does not grow for the life of the process.
func (r *Runtime) doneSweeping(outpoint string) {
	r.sweepMu.Lock()
	defer r.sweepMu.Unlock()
	delete(r.sweeping, outpoint)
}

// splitOutpoint reads "txid:vout".
func splitOutpoint(s string) (string, uint32, error) {
	txid, idx, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return "", 0, fmt.Errorf("%q is not an outpoint", s)
	}
	vout, err := strconv.ParseUint(idx, 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("%q has no output index", s)
	}
	if len(txid) != 64 {
		return "", 0, fmt.Errorf("%q does not name a transaction", s)
	}
	return txid, uint32(vout), nil
}

// payScriptFor turns the destination the bridge named into a script.
func payScriptFor(addr string, params stdaddr.AddressParams) ([]byte, error) {
	a, err := stdaddr.DecodeAddress(strings.TrimSpace(addr), params)
	if err != nil {
		return nil, fmt.Errorf("the destination is not an address on this chain: %w", err)
	}
	_, script := a.PaymentScript()
	return script, nil
}
