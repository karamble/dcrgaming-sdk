// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package bridge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/decred/dcrd/dcrjson/v4"
	"github.com/decred/dcrd/dcrutil/v4"
	chainjson "github.com/decred/dcrd/rpc/jsonrpc/types/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/txscript/v4/stdscript"
	"github.com/decred/dcrd/wire"
)

// The fake host is a node, a wallet, a Bison Relay client and an operator kept
// in memory: enough of each for a real bridge to fund, sign, publish and
// reconcile against, counting what the bridge asked of them.

var fakeParams = chaincfg.MainNetParams()

// fakeChain is a node: blocks, a mempool and every transaction it was given.
type fakeChain struct {
	mu      sync.Mutex
	blocks  []chainhash.Hash
	txs     map[chainhash.Hash]*fakeTx
	mempool []chainhash.Hash
	credits int
}

type fakeTx struct {
	tx     *wire.MsgTx
	height int64 // zero while in the mempool
}

func newFakeChain(height int64) *fakeChain {
	c := &fakeChain{txs: map[chainhash.Hash]*fakeTx{}}
	for h := int64(0); h <= height; h++ {
		c.blocks = append(c.blocks, chainhash.HashH([]byte(fmt.Sprintf("fake block %d", h))))
	}
	return c
}

func (c *fakeChain) tipLocked() int64 { return int64(len(c.blocks) - 1) }

// mine adds n blocks, the first taking everything in the mempool.
func (c *fakeChain) mine(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := 0; i < n; i++ {
		height := int64(len(c.blocks))
		c.blocks = append(c.blocks, chainhash.HashH([]byte(fmt.Sprintf("fake block %d", height))))
		for _, h := range c.mempool {
			c.txs[h].height = height
		}
		c.mempool = nil
	}
}

func (c *fakeChain) mempoolSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.mempool)
}

func (c *fakeChain) height() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tipLocked()
}

// credit mines a transaction paying atoms to pkScript.
func (c *fakeChain) credit(pkScript []byte, atoms int64) wire.OutPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credits++
	tx := wire.NewMsgTx()
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.HashH([]byte(fmt.Sprintf("fake credit %d", c.credits)))}, atoms, nil))
	tx.AddTxOut(wire.NewTxOut(atoms, pkScript))
	c.txs[tx.TxHash()] = &fakeTx{tx: tx, height: c.tipLocked()}
	return wire.OutPoint{Hash: tx.TxHash(), Index: 0, Tree: wire.TxTreeRegular}
}

// accept takes a signed transaction into the mempool.
func (c *fakeChain) accept(tx *wire.MsgTx) (chainhash.Hash, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := tx.TxHash()
	if _, ok := c.txs[h]; ok {
		return h, nil
	}
	for i, in := range tx.TxIn {
		prev := c.outputLocked(in.PreviousOutPoint)
		if prev == nil {
			return h, fmt.Errorf("input %v is unknown", in.PreviousOutPoint)
		}
		if c.spenderLocked(in.PreviousOutPoint, false) != nil {
			return h, fmt.Errorf("input %v is already spent", in.PreviousOutPoint)
		}
		vm, err := txscript.NewEngine(prev.PkScript, tx, i, txscript.ScriptVerifyCheckSequenceVerify|txscript.ScriptVerifyCleanStack|txscript.ScriptVerifySigPushOnly, prev.Version, nil)
		if err == nil {
			err = vm.Execute()
		}
		if err != nil {
			return h, fmt.Errorf("input %d does not spend %v: %w", i, in.PreviousOutPoint, err)
		}
	}
	c.txs[h] = &fakeTx{tx: tx}
	c.mempool = append(c.mempool, h)
	return h, nil
}

func (c *fakeChain) outputLocked(op wire.OutPoint) *wire.TxOut {
	t := c.txs[op.Hash]
	if t == nil || int(op.Index) >= len(t.tx.TxOut) {
		return nil
	}
	return t.tx.TxOut[op.Index]
}

// spenderLocked is the transaction spending op, counting the mempool only if
// asked to.
func (c *fakeChain) spenderLocked(op wire.OutPoint, minedOnly bool) *fakeTx {
	for _, t := range c.txs {
		if minedOnly && t.height == 0 {
			continue
		}
		for _, in := range t.tx.TxIn {
			if in.PreviousOutPoint == op {
				return t
			}
		}
	}
	return nil
}

func (c *fakeChain) confirmationsLocked(t *fakeTx) int64 {
	if t.height == 0 {
		return 0
	}
	return c.tipLocked() - t.height + 1
}

func noTxInfo() error {
	return &dcrjson.RPCError{Code: dcrjson.ErrRPCNoTxInfo, Message: "No information available about transaction"}
}

func (c *fakeChain) GetBestBlock(context.Context) (*chainhash.Hash, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.blocks[c.tipLocked()]
	return &h, c.tipLocked(), nil
}

func (c *fakeChain) GetBlockChainInfo(context.Context) (*chainjson.GetBlockChainInfoResult, error) {
	return &chainjson.GetBlockChainInfoResult{Chain: fakeParams.Name, Blocks: c.height()}, nil
}

func (c *fakeChain) GetBlockCount(context.Context) (int64, error) { return c.height(), nil }

func (c *fakeChain) GetBlockHash(_ context.Context, height int64) (*chainhash.Hash, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if height < 0 || height > c.tipLocked() {
		return nil, fmt.Errorf("block height %d out of range", height)
	}
	h := c.blocks[height]
	return &h, nil
}

func (c *fakeChain) GetBlockHeaderVerbose(_ context.Context, hash *chainhash.Hash) (*chainjson.GetBlockHeaderVerboseResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for h, b := range c.blocks {
		if b == *hash {
			return &chainjson.GetBlockHeaderVerboseResult{Hash: b.String(), Height: uint32(h), Confirmations: c.tipLocked() - int64(h) + 1}, nil
		}
	}
	return nil, fmt.Errorf("block %v not found", hash)
}

func (c *fakeChain) GetInfo(context.Context) (*chainjson.InfoChainResult, error) {
	return &chainjson.InfoChainResult{Blocks: c.height(), TxIndex: true}, nil
}

func (c *fakeChain) GetRawMempool(context.Context, chainjson.GetRawMempoolTxTypeCmd) ([]*chainhash.Hash, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*chainhash.Hash, 0, len(c.mempool))
	for i := range c.mempool {
		h := c.mempool[i]
		out = append(out, &h)
	}
	return out, nil
}

func (c *fakeChain) GetRawTransaction(_ context.Context, hash *chainhash.Hash) (*dcrutil.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.txs[*hash]
	if t == nil {
		return nil, noTxInfo()
	}
	return dcrutil.NewTx(t.tx), nil
}

func (c *fakeChain) GetRawTransactionVerbose(_ context.Context, hash *chainhash.Hash) (*chainjson.TxRawResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.txs[*hash]
	if t == nil {
		return nil, noTxInfo()
	}
	out := &chainjson.TxRawResult{Txid: hash.String(), Confirmations: c.confirmationsLocked(t)}
	if t.height > 0 {
		out.BlockHash, out.BlockHeight = c.blocks[t.height].String(), t.height
	}
	return out, nil
}

// GetTxOut answers as dcrd does: an output spent only in the mempool is still
// reported, and one still in the mempool only when asked for.
func (c *fakeChain) GetTxOut(_ context.Context, hash *chainhash.Hash, index uint32, _ int8, mempool bool) (*chainjson.GetTxOutResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.txs[*hash]
	op := wire.OutPoint{Hash: *hash, Index: index, Tree: wire.TxTreeRegular}
	out := c.outputLocked(op)
	if out == nil || (t.height == 0 && !mempool) || c.spenderLocked(op, true) != nil {
		return nil, nil
	}
	typ, addrs := stdscript.ExtractAddrs(out.Version, out.PkScript, fakeParams)
	names := make([]string, 0, len(addrs))
	for _, a := range addrs {
		names = append(names, a.String())
	}
	return &chainjson.GetTxOutResult{
		BestBlock:     c.blocks[c.tipLocked()].String(),
		Confirmations: c.confirmationsLocked(t),
		Value:         dcrutil.Amount(out.Value).ToCoin(),
		ScriptPubKey: chainjson.ScriptPubKeyResult{
			Hex: hex.EncodeToString(out.PkScript), Type: typ.String(),
			Addresses: names, Version: out.Version,
		},
	}, nil
}

// fakeGamingAccount is the wallet account the game is bound to.
const fakeGamingAccount = 1

// fakeWallet holds keys in two accounts and spends the chain's coins.
type fakeWallet struct {
	mu      sync.Mutex
	chain   *fakeChain
	xpub    string
	pass    string
	keys    map[string]fakeKey
	used    map[wire.OutPoint]bool
	calls   map[string]int
	scripts []string
	relayed []chainhash.Hash
}

type fakeKey struct {
	priv    *secp256k1.PrivateKey
	account uint32
}

func newFakeWallet(chain *fakeChain, name string) *fakeWallet {
	return &fakeWallet{
		chain: chain, xpub: "fake xpub of " + name, pass: "pass",
		keys: map[string]fakeKey{}, used: map[wire.OutPoint]bool{}, calls: map[string]int{},
	}
}

func (w *fakeWallet) called(method string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[method]
}

func (w *fakeWallet) callCounts() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.calls)
}

func (w *fakeWallet) count(method string) {
	w.mu.Lock()
	w.calls[method]++
	w.mu.Unlock()
}

func (w *fakeWallet) newAddressLocked(account uint32) (stdaddr.Address, error) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(stdaddr.Hash160(priv.PubKey().SerializeCompressed()), fakeParams)
	if err != nil {
		return nil, err
	}
	w.keys[addr.String()] = fakeKey{priv: priv, account: account}
	return addr, nil
}

// fund gives the account a mined coin of atoms.
func (w *fakeWallet) fund(account uint32, atoms int64) (wire.OutPoint, error) {
	w.mu.Lock()
	addr, err := w.newAddressLocked(account)
	w.mu.Unlock()
	if err != nil {
		return wire.OutPoint{}, err
	}
	_, pk := addr.PaymentScript()
	return w.chain.credit(pk, atoms), nil
}

func (w *fakeWallet) ownerLocked(pkScript []byte, version uint16) (fakeKey, bool) {
	_, addrs := stdscript.ExtractAddrs(version, pkScript, fakeParams)
	if len(addrs) != 1 {
		return fakeKey{}, false
	}
	key, ok := w.keys[addrs[0].String()]
	return key, ok
}

// coinLocked picks an unused mined coin of the account worth at least atoms.
func (w *fakeWallet) coinLocked(account uint32, atoms int64) (wire.OutPoint, int64, error) {
	w.chain.mu.Lock()
	defer w.chain.mu.Unlock()
	for h, t := range w.chain.txs {
		if t.height == 0 {
			continue
		}
		for i, out := range t.tx.TxOut {
			op := wire.OutPoint{Hash: h, Index: uint32(i), Tree: wire.TxTreeRegular}
			key, mine := w.ownerLocked(out.PkScript, out.Version)
			if !mine || key.account != account || w.used[op] || out.Value < atoms || w.chain.spenderLocked(op, false) != nil {
				continue
			}
			w.used[op] = true
			return op, out.Value, nil
		}
	}
	return wire.OutPoint{}, 0, fmt.Errorf("account %d has no coin worth %d atoms", account, atoms)
}

func (w *fakeWallet) CheckSigning(context.Context) error { return nil }

func (w *fakeWallet) Accounts(context.Context) ([]Account, error) {
	return []Account{{Name: "default", Number: 0}, {Name: "gaming", Number: fakeGamingAccount}}, nil
}

func (w *fakeWallet) ReservedAccount(name string) bool { return name == "imported" }

func (w *fakeWallet) AccountXPub(_ context.Context, account uint32) (string, error) {
	return fmt.Sprintf("%s account %d", w.xpub, account), nil
}

func (w *fakeWallet) ValidateAddress(_ context.Context, address string) (AddressInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if key, ok := w.keys[address]; ok {
		return AddressInfo{IsValid: true, IsMine: true, AccountNumber: key.account, PubKey: key.priv.PubKey().SerializeCompressed()}, nil
	}
	if _, err := stdaddr.DecodeAddress(address, fakeParams); err != nil {
		return AddressInfo{}, nil
	}
	return AddressInfo{IsValid: true}, nil
}

func (w *fakeWallet) NextInternalAddress(_ context.Context, account uint32) (string, error) {
	w.count("NextInternalAddress")
	w.mu.Lock()
	defer w.mu.Unlock()
	addr, err := w.newAddressLocked(account)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

func (w *fakeWallet) NextExternalAddress(_ context.Context, account uint32) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	addr, err := w.newAddressLocked(account)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

func (w *fakeWallet) ImportScript(_ context.Context, scriptHex string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.scripts = append(w.scripts, scriptHex)
	return nil
}

func (w *fakeWallet) imported() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.scripts...)
}

// fakeFeeAtoms is what every transaction the wallet builds pays.
const fakeFeeAtoms = 5000

func (w *fakeWallet) Construct(_ context.Context, account uint32, address string, atoms int64) ([]byte, error) {
	w.count("Construct")
	dest, err := stdaddr.DecodeAddress(address, fakeParams)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	op, value, err := w.coinLocked(account, atoms+fakeFeeAtoms)
	if err != nil {
		return nil, err
	}
	tx := wire.NewMsgTx()
	tx.AddTxIn(wire.NewTxIn(&op, value, nil))
	_, pk := dest.PaymentScript()
	tx.AddTxOut(wire.NewTxOut(atoms, pk))
	if change := value - atoms - fakeFeeAtoms; change > 0 {
		back, err := w.newAddressLocked(account)
		if err != nil {
			return nil, err
		}
		_, backPk := back.PaymentScript()
		tx.AddTxOut(wire.NewTxOut(change, backPk))
	}
	return tx.Bytes()
}

func (w *fakeWallet) SignTransaction(_ context.Context, account uint32, unsigned, passphrase []byte) ([]byte, error) {
	w.count("SignTransaction")
	if string(passphrase) != w.pass {
		return nil, errors.New("wrong passphrase")
	}
	var tx wire.MsgTx
	if err := tx.FromBytes(unsigned); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, in := range tx.TxIn {
		w.chain.mu.Lock()
		prev := w.chain.outputLocked(in.PreviousOutPoint)
		w.chain.mu.Unlock()
		if prev == nil {
			return nil, fmt.Errorf("input %d is unknown", i)
		}
		key, mine := w.ownerLocked(prev.PkScript, prev.Version)
		if !mine || key.account != account {
			return nil, fmt.Errorf("input %d is not this account's", i)
		}
		hash, err := txscript.CalcSignatureHash(prev.PkScript, txscript.SigHashAll, &tx, i, nil)
		if err != nil {
			return nil, err
		}
		sig := append(ecdsa.Sign(key.priv, hash).Serialize(), byte(txscript.SigHashAll))
		script, err := txscript.NewScriptBuilder().AddData(sig).AddData(key.priv.PubKey().SerializeCompressed()).Script()
		if err != nil {
			return nil, err
		}
		tx.TxIn[i].SignatureScript = script
	}
	return tx.Bytes()
}

func (w *fakeWallet) WithUnlockedAccount(_ context.Context, _ uint32, passphrase []byte, fn func() error) error {
	w.count("WithUnlockedAccount")
	if string(passphrase) != w.pass {
		return errors.New("wrong passphrase")
	}
	return fn()
}

func (w *fakeWallet) SignHash(_ context.Context, address string, hash []byte) ([]byte, []byte, error) {
	w.count("SignHash")
	w.mu.Lock()
	defer w.mu.Unlock()
	key, ok := w.keys[address]
	if !ok {
		return nil, nil, fmt.Errorf("no key for %s", address)
	}
	return key.priv.PubKey().SerializeCompressed(), ecdsa.Sign(key.priv, hash).Serialize(), nil
}

// relay hands a transaction to the node, noting it was tried whether or not
// the node takes it.
func (w *fakeWallet) relay(signed []byte) (string, error) {
	var tx wire.MsgTx
	if err := tx.FromBytes(signed); err != nil {
		return "", err
	}
	w.mu.Lock()
	w.relayed = append(w.relayed, tx.TxHash())
	w.mu.Unlock()
	h, err := w.chain.accept(&tx)
	return h.String(), err
}

func (w *fakeWallet) tried(h chainhash.Hash) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.relayed, h)
}

func (w *fakeWallet) Publish(_ context.Context, signed []byte) (string, error) {
	w.count("Publish")
	return w.relay(signed)
}

func (w *fakeWallet) Broadcast(_ context.Context, signed []byte) (string, error) {
	w.count("Broadcast")
	return w.relay(signed)
}

func (w *fakeWallet) Transaction(_ context.Context, hash chainhash.Hash) (WalletTx, bool, error) {
	c := w.chain
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.txs[hash]
	if t == nil {
		return WalletTx{}, false, nil
	}
	raw, err := t.tx.Bytes()
	if err != nil {
		return WalletTx{}, false, err
	}
	out := WalletTx{Raw: raw, Confirmations: int32(c.confirmationsLocked(t))}
	if t.height > 0 {
		b := c.blocks[t.height]
		out.BlockHash = &b
	}
	return out, true, nil
}

func (w *fakeWallet) MinedTransactions(_ context.Context, start int32, fn func(int32, [][]byte) error) error {
	c := w.chain
	c.mu.Lock()
	byHeight := map[int64][][]byte{}
	for _, t := range c.txs {
		if t.height >= int64(start) && t.height > 0 {
			raw, err := t.tx.Bytes()
			if err != nil {
				c.mu.Unlock()
				return err
			}
			byHeight[t.height] = append(byHeight[t.height], raw)
		}
	}
	tip := c.tipLocked()
	c.mu.Unlock()
	for h := int64(start); h <= tip; h++ {
		if err := fn(int32(h), byHeight[h]); err != nil {
			return err
		}
	}
	return nil
}

// fakeRelay is a Bison Relay client that records what it sent and hands it to
// the other members' bridges, in order, as their clients would.
type fakeRelay struct {
	mu    sync.Mutex
	uid   [32]byte
	nick  string
	sent  []fakeGroupMessage
	peers []*Bridge
	queue chan fakeGroupMessage
}

type fakeGroupMessage struct {
	gcid [32]byte
	text string
}

func newFakeRelay(nick string) *fakeRelay {
	r := &fakeRelay{nick: nick}
	copy(r.uid[:], chainhash.HashB([]byte("fake uid of "+nick)))
	return r
}

// joinGroup hands everything this client sends from now on to peers.
func (r *fakeRelay) joinGroup(ctx context.Context, peers ...*Bridge) {
	r.mu.Lock()
	r.peers = append(r.peers, peers...)
	if r.queue == nil {
		r.queue = make(chan fakeGroupMessage, 256)
		go r.deliver(ctx)
	}
	r.mu.Unlock()
}

func (r *fakeRelay) deliver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-r.queue:
			r.mu.Lock()
			peers := append([]*Bridge(nil), r.peers...)
			r.mu.Unlock()
			for _, p := range peers {
				for p.ReceiveGroupMessage(GroupMessage{GCID: m.gcid, From: r.uid, Text: m.text, Time: time.Now()}) != nil {
					select {
					case <-ctx.Done():
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
		}
	}
}

func (r *fakeRelay) sentTo(gcid [32]byte) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.sent {
		if m.gcid == gcid {
			out = append(out, m.text)
		}
	}
	return out
}

func (r *fakeRelay) Identity(context.Context) ([32]byte, string, error) { return r.uid, r.nick, nil }

func (r *fakeRelay) SendGroupMessage(_ context.Context, gcid [32]byte, text string) error {
	m := fakeGroupMessage{gcid: gcid, text: text}
	r.mu.Lock()
	r.sent = append(r.sent, m)
	queue := r.queue
	r.mu.Unlock()
	if queue != nil {
		queue <- m
	}
	return nil
}

func (r *fakeRelay) GroupHistory(_ context.Context, gcid [32]byte, page, pageSize int) ([]GroupEntry, error) {
	var out []GroupEntry
	for _, text := range r.sentTo(gcid) {
		out = append(out, GroupEntry{From: r.nick, Message: text})
	}
	start := page * pageSize
	if start >= len(out) {
		return nil, nil
	}
	return out[start:min(start+pageSize, len(out))], nil
}

// fakeOperator is protected until a test says otherwise.
type fakeOperator struct {
	unprotected atomic.Bool
}

func (o *fakeOperator) Protected() bool        { return !o.unprotected.Load() }
func (o *fakeOperator) PresenceChanged(string) {}

// fakeHost is one wallet app: its node, wallet, Bison Relay client and
// operator.
type fakeHost struct {
	chain    *fakeChain
	wallet   *fakeWallet
	relay    *fakeRelay
	operator *fakeOperator
}

func newFakeHost(chain *fakeChain, name string) *fakeHost {
	return &fakeHost{
		chain: chain, wallet: newFakeWallet(chain, name),
		relay: newFakeRelay(name), operator: &fakeOperator{},
	}
}

func (h *fakeHost) host() Host {
	return Host{
		Node:     func() Chain { return h.chain },
		Wallet:   h.wallet,
		Relay:    h.relay,
		Operator: h.operator,
	}
}
