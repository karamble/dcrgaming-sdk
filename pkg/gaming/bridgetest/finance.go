package bridgetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
	"github.com/karamble/dcrgaming-sdk/pkg/finance"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

type fakeDeposit struct {
	Owner string
	Terms finance.Terms
	Reply *gamingpb.PreparedDeposit
	Spend string
}
type fakePayout struct {
	Tx        *wire.MsgTx
	Inputs    []finance.Input
	Status    *gamingpb.PayoutStatusReply
	Sigs      map[string][][]byte
	Proposers map[string]bool
}

func (b *Bridge) financialKey(owner, sid string) *secp256k1.PrivateKey {
	if b.financialKeys == nil {
		b.financialKeys = map[string]*secp256k1.PrivateKey{}
	}
	id := owner + ":" + sid
	if key := b.financialKeys[id]; key != nil {
		return key
	}
	// Deterministic test-only wallet key. Never used by a production bridge.
	seed := sha256.Sum256([]byte("bridgetest-wallet-v2:" + id))
	key := secp256k1.PrivKeyFromBytes(seed[:])
	b.financialKeys[id] = key
	return key
}
func (b *Bridge) FinancialKey(ctx context.Context, req *gamingpb.FinancialKeyRequest) (*gamingpb.FinancialKeyReply, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := b.financialKey(callerCN(ctx), req.GetSid())
	pub := key.PubKey().SerializeCompressed()
	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(stdaddr.Hash160(pub), b.opts.Params)
	if err != nil {
		return nil, err
	}
	return &gamingpb.FinancialKeyReply{PublicKey: hex.EncodeToString(pub), PayoutAddress: addr.String()}, nil
}
func (b *Bridge) PrepareDeposit(ctx context.Context, req *gamingpb.PrepareDepositRequest) (*gamingpb.PreparedDeposit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	owner := callerCN(ctx)
	key := b.financialKey(owner, req.Sid)
	terms := finance.Terms{Version: finance.Version, Game: b.opts.Game, Network: b.opts.Network, Table: req.Sid, Kind: req.Kind, Atoms: req.AmountAtoms, LockBlocks: req.LockBlocks, Identity: req.IdentityKey, Recovery: hex.EncodeToString(key.PubKey().SerializeCompressed()), Members: req.Members}
	terms, err := terms.Canonical()
	if err != nil {
		return nil, err
	}
	script, err := terms.Script()
	if err != nil {
		return nil, err
	}
	address, pk, err := terms.Output(b.opts.Params)
	if err != nil {
		return nil, err
	}
	id := owner + ":" + req.Sid + ":" + req.Kind
	if b.deposits == nil {
		b.deposits = map[string]fakeDeposit{}
	}
	if prior, ok := b.deposits[id]; ok {
		if prior.Reply.Address != address || prior.Terms.Atoms != terms.Atoms {
			return nil, fmt.Errorf("deposit terms changed")
		}
		return prior.Reply, nil
	}
	reply := &gamingpb.PreparedDeposit{Id: id, Address: address, RedeemScript: hex.EncodeToString(script), PkScript: hex.EncodeToString(pk), RecoveryKey: terms.Recovery}
	b.deposits[id] = fakeDeposit{Owner: owner, Terms: terms, Reply: reply}
	return reply, nil
}

// SetPayoutVerdict represents an operator's explicit consent in lifecycle tests.
// The fake does not replace dcrpulse's adversarial financial-authority tests.
func (b *Bridge) SetPayoutVerdict(v Verdict) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.payoutVerdict = v
	if v != Approve {
		return nil
	}
	for _, held := range b.payouts {
		for _, key := range b.financialKeys {
			pub := hex.EncodeToString(key.PubKey().SerializeCompressed())
			if held.Proposers[pub] {
				if err := b.approvePayout(held, key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (b *Bridge) ProposePayout(ctx context.Context, req *gamingpb.ProposePayoutRequest) (*gamingpb.PayoutStatusReply, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := finance.Payout{Table: req.Sid}
	destinations := map[string]string{}
	for _, want := range req.Inputs {
		if want == nil {
			return nil, fmt.Errorf("missing payout input")
		}
		found := false
		for _, dep := range b.deposits {
			if dep.Terms.Kind != "stake" || dep.Terms.Table != req.Sid || dep.Terms.Recovery != want.OwnerKey {
				continue
			}
			var point wire.OutPoint
			hash, err := chainhash.NewHashFromStr(want.Txid)
			if err != nil {
				return nil, err
			}
			point.Hash = *hash
			point.Index = want.Vout
			p.Inputs = append(p.Inputs, finance.Input{Terms: dep.Terms, Outpoint: point})
			pub, _ := hex.DecodeString(want.OwnerKey)
			addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(stdaddr.Hash160(pub), b.opts.Params)
			if err != nil {
				return nil, err
			}
			destinations[want.OwnerKey] = addr.String()
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("unknown payout member")
		}
	}
	for _, pay := range req.Payments {
		if pay == nil {
			return nil, fmt.Errorf("missing payout payment")
		}
		p.Payments = append(p.Payments, finance.Payment{Key: pay.OwnerKey, Atoms: pay.AmountAtoms})
	}
	built, err := finance.BuildPayout(p, destinations, b.opts.Params)
	if err != nil {
		return nil, err
	}
	tx, inputs := built.Transaction, built.Inputs
	id := tx.TxHash().String()
	if b.payouts == nil {
		b.payouts = map[string]*fakePayout{}
	}
	held := b.payouts[id]
	key := b.financialKey(callerCN(ctx), req.Sid)
	pub := hex.EncodeToString(key.PubKey().SerializeCompressed())
	if destinations[pub] == "" {
		return nil, fmt.Errorf("caller not in payout")
	}
	if held != nil && held.Status.State == "confirmed" {
		held.Proposers[pub] = true
		return copyPayoutStatus(held.Status), nil
	}
	for _, input := range inputs {
		_, pk, err := input.Terms.Output(b.opts.Params)
		if err != nil {
			return nil, err
		}
		facts, ok := b.utxos[outKey(input.Outpoint.Hash.String(), input.Outpoint.Index)]
		if !ok || facts.Value != input.Terms.Atoms || hex.EncodeToString(facts.PkScript) != hex.EncodeToString(pk) || facts.ConfirmAt > b.height {
			return nil, fmt.Errorf("unfunded or unconfirmed payout input")
		}
	}
	if held == nil {
		held = &fakePayout{Tx: tx, Inputs: inputs, Proposers: map[string]bool{}, Sigs: map[string][][]byte{}, Status: &gamingpb.PayoutStatusReply{Id: id, Sid: req.Sid, State: "awaiting_approval", Required: uint32(len(inputs))}}
		b.payouts[id] = held
	}
	held.Proposers[pub] = true
	if b.payoutVerdict != Approve {
		return copyPayoutStatus(held.Status), nil
	}
	if err := b.approvePayout(held, key); err != nil {
		return nil, err
	}
	return copyPayoutStatus(held.Status), nil
}

// approvePayout models an operator answering an existing pending proposal.
// Caller holds b.mu. Production approval lives entirely in dcrpulse.
func (b *Bridge) approvePayout(held *fakePayout, key *secp256k1.PrivateKey) error {
	if held.Status.State == "confirmed" {
		return nil
	}
	tx, inputs := held.Tx, held.Inputs
	pub := hex.EncodeToString(key.PubKey().SerializeCompressed())
	var err error
	sigs := make([][]byte, len(inputs))
	for i, input := range inputs {
		hash, err := finance.SignatureHash(tx, i, input)
		if err != nil {
			return err
		}
		sigs[i] = ecdsa.Sign(key, hash).Serialize()
	}
	held.Sigs[pub] = sigs
	held.Status.Signatures = uint32(len(held.Sigs))
	held.Status.State = "awaiting_signatures"
	if len(held.Sigs) == len(inputs) {
		for i, input := range inputs {
			byKey := map[string][]byte{}
			for signer, sigs := range held.Sigs {
				byKey[signer] = sigs[i]
			}
			held.Tx.TxIn[i].SignatureScript, err = finance.SettlementWitness(held.Tx, i, input, byKey)
			if err != nil {
				return err
			}
		}
		if err = finance.VerifySpend(held.Tx, inputs, b.opts.Params); err != nil {
			return err
		}
		raw, _ := held.Tx.Bytes()
		b.rawtx[held.Status.Id] = raw
		b.sent[held.Status.Id]++
		for _, input := range inputs {
			delete(b.utxos, outKey(input.Outpoint.Hash.String(), input.Outpoint.Index))
		}
		held.Status.State = "confirmed"
		held.Status.Txid = held.Status.Id
	}
	return nil
}
func copyPayoutStatus(p *gamingpb.PayoutStatusReply) *gamingpb.PayoutStatusReply {
	return &gamingpb.PayoutStatusReply{Id: p.Id, Sid: p.Sid, State: p.State, Txid: p.Txid, Signatures: p.Signatures, Required: p.Required}
}
func (b *Bridge) PayoutStatus(ctx context.Context, req *gamingpb.PayoutStatusRequest) (*gamingpb.PayoutStatusReply, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	held := b.payouts[req.Id]
	if held == nil {
		return nil, fmt.Errorf("unknown payout")
	}
	key := b.financialKey(callerCN(ctx), held.Status.Sid)
	if !held.Proposers[hex.EncodeToString(key.PubKey().SerializeCompressed())] {
		return nil, fmt.Errorf("payout not proposed by caller")
	}
	return copyPayoutStatus(held.Status), nil
}

func (b *Bridge) FinancialState(ctx context.Context, req *gamingpb.FinancialStateRequest) (*gamingpb.FinancialStateReply, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	owner := callerCN(ctx)
	reply := &gamingpb.FinancialStateReply{Sid: req.Sid}
	for id, dep := range b.deposits {
		if dep.Owner != owner || dep.Terms.Table != req.Sid {
			continue
		}
		state, outpoint := "prepared", ""
		var confirmations int64
		if spend := b.spends[dep.Spend]; spend != nil && spend.GetTxid() != "" {
			outpoint = outKey(spend.GetTxid(), 1)
			if u, ok := b.utxos[outpoint]; ok {
				state = "mempool"
				if b.height >= u.ConfirmAt {
					state = "confirmed"
					confirmations = b.height - u.ConfirmAt + 1
				}
			} else {
				state = "spent"
			}
		}
		reply.Deposits = append(reply.Deposits, &gamingpb.DepositStatus{Id: id, Kind: dep.Terms.Kind, State: state, Outpoint: outpoint, AmountAtoms: dep.Terms.Atoms, LockBlocks: dep.Terms.LockBlocks, Confirmations: confirmations})
	}
	return reply, nil
}
