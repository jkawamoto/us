package wallet

import (
	"encoding/binary"
	"io"
	"time"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
	"golang.org/x/crypto/blake2b"
)

// An AddressOwner claims ownership of addresses.
type AddressOwner interface {
	OwnsAddress(addr types.UnlockHash) bool
}

// A ChainStore stores ProcessedConsensusChanges.
type ChainStore interface {
	ApplyConsensusChange(reverted, applied ProcessedConsensusChange, id modules.ConsensusChangeID)
}

// A Store stores information needed by a generic wallet.
type Store interface {
	ChainStore
	BlockRewards(n int) []BlockReward
	ConsensusChangeID() modules.ConsensusChangeID
	ChainHeight() types.BlockHeight
	FileContracts(n int) []FileContract
	FileContractHistory(id types.FileContractID) []FileContract
	LimboTransactions() []LimboTransaction
	AddToLimbo(txn types.Transaction)
	RemoveFromLimbo(id types.TransactionID)
	Memo(txid types.TransactionID) []byte
	SetMemo(txid types.TransactionID, memo []byte)
	Transaction(id types.TransactionID) (types.Transaction, bool)
	Transactions(n int) []types.TransactionID
	TransactionsByAddress(addr types.UnlockHash, n int) []types.TransactionID
	UnspentOutputs() []UnspentOutput
}

// A SeedStore stores information needed by a SeedWallet.
type SeedStore interface {
	Store
	SeedIndex() uint64
	SetSeedIndex(index uint64)
}

// A WatchOnlyStore stores information needed by a WatchOnlyWallet. For
// convenience, it also implements AddressOwner.
type WatchOnlyStore interface {
	Store
	AddressOwner
	Addresses() []types.UnlockHash
	AddAddress(addr types.UnlockHash, info []byte)
	AddressInfo(addr types.UnlockHash) []byte
	RemoveAddress(addr types.UnlockHash)
}

// A ProcessedConsensusChange is a condensation of a modules.ConsensusChange,
// containing only the data relevant to certain addresses, and intended to be
// processed by an atomic unit.
type ProcessedConsensusChange struct {
	Outputs             []UnspentOutput
	Transactions        []types.Transaction
	AddressTransactions map[types.UnlockHash][]types.TransactionID
	BlockRewards        []BlockReward
	FileContracts       []FileContract
	BlockCount          int
	CCID                modules.ConsensusChangeID
}

// StandardUnlockConditions are the unlock conditions for a standard address:
// one public key, no timelock.
func StandardUnlockConditions(pk types.SiaPublicKey) types.UnlockConditions {
	return types.UnlockConditions{
		PublicKeys:         []types.SiaPublicKey{pk},
		SignaturesRequired: 1,
	}
}

// StandardAddress returns the UnlockHash of a set of StandardUnlockConditions.
func StandardAddress(pk types.SiaPublicKey) types.UnlockHash {
	// To avoid allocating, compute the UnlockHash manually. An UnlockHash is
	// the Merkle root of UnlockConditions, which comprise a timelock, a set
	// of public keys, and the number of signatures required. Since the
	// standard UnlockConditions use a single public key, the Merkle tree is:
	//
	//           ┌─────────┴──────────┐
	//     ┌─────┴─────┐              │
	//  timelock     pubkey     sigsrequired
	//
	// This implies a total of 5 blake2b hashes: 3 leaves and 2 nodes.
	// However, in the standard UnlockConditions, the timelock and
	// sigsrequired are always the same (0 and 1, respectively), so we can
	// precompute these hashes, bringing the total to 3 blake2b hashes.

	// calculate the leaf hash for the pubkey.
	buf := make([]byte, 65)
	buf[0] = 0x00 // Merkle tree leaf prefix
	copy(buf[1:], pk.Algorithm[:])
	binary.LittleEndian.PutUint64(buf[17:], uint64(len(pk.Key)))
	buf = append(buf[:25], pk.Key...) // won't realloc for ed25519 keys
	pubkeyHash := blake2b.Sum256(buf)

	// blake2b(0x00 | uint64(0))
	timelockHash := []byte{
		0x51, 0x87, 0xb7, 0xa8, 0x02, 0x1b, 0xf4, 0xf2,
		0xc0, 0x04, 0xea, 0x3a, 0x54, 0xcf, 0xec, 0xe1,
		0x75, 0x4f, 0x11, 0xc7, 0x62, 0x4d, 0x23, 0x63,
		0xc7, 0xf4, 0xcf, 0x4f, 0xdd, 0xd1, 0x44, 0x1e,
	}
	// blake2b(0x00 | uint64(1))
	sigsrequiredHash := []byte{
		0xb3, 0x60, 0x10, 0xeb, 0x28, 0x5c, 0x15, 0x4a,
		0x8c, 0xd6, 0x30, 0x84, 0xac, 0xbe, 0x7e, 0xac,
		0x0c, 0x4d, 0x62, 0x5a, 0xb4, 0xe1, 0xa7, 0x6e,
		0x62, 0x4a, 0x87, 0x98, 0xcb, 0x63, 0x49, 0x7b,
	}

	buf = buf[:65]
	buf[0] = 0x01 // Merkle tree node prefix
	copy(buf[1:], timelockHash)
	copy(buf[33:], pubkeyHash[:])
	tlpkHash := blake2b.Sum256(buf)
	copy(buf[1:], tlpkHash[:])
	copy(buf[33:], sigsrequiredHash)
	return blake2b.Sum256(buf)
}

// CalculateUnlockHash calculates the UnlockHash of a set of UnlockConditions.
// It calls StandardAddress on "standard" UnlockConditions, falling back to the
// UnlockHash method otherwise. Since the vast majority of UnlockConditions are
// standard, this results in faster average computation.
func CalculateUnlockHash(uc types.UnlockConditions) types.UnlockHash {
	if uc.Timelock == 0 && len(uc.PublicKeys) == 1 && uc.SignaturesRequired == 1 {
		return StandardAddress(uc.PublicKeys[0])
	}
	return uc.UnlockHash()
}

// StandardTransactionSignature is the most common form of TransactionSignature.
// It covers the entire transaction and references the first (typically the
// only) public key.
func StandardTransactionSignature(id crypto.Hash) types.TransactionSignature {
	return types.TransactionSignature{
		ParentID:       id,
		CoveredFields:  types.FullCoveredFields,
		PublicKeyIndex: 0,
	}
}

// An UnspentOutput is a SiacoinOutput along with its ID.
type UnspentOutput struct {
	types.SiacoinOutput
	ID types.SiacoinOutputID
}

// MarshalSia implements encoding.SiaMarshaler.
func (o UnspentOutput) MarshalSia(w io.Writer) error {
	return encoding.NewEncoder(w).EncodeAll(o.SiacoinOutput, o.ID)
}

// UnmarshalSia implements encoding.SiaUnmarshaler.
func (o *UnspentOutput) UnmarshalSia(r io.Reader) error {
	return encoding.NewDecoder(r, encoding.DefaultAllocLimit).DecodeAll(&o.SiacoinOutput, &o.ID)
}

// A ValuedInput is a SiacoinInput along with its value. Seen another way, it is
// an UnspentOutput that knows its UnlockConditions.
type ValuedInput struct {
	types.SiacoinInput
	Value types.Currency
}

// MarshalSia implements encoding.SiaMarshaler.
func (i ValuedInput) MarshalSia(w io.Writer) error {
	return encoding.NewEncoder(w).EncodeAll(i.SiacoinInput, i.Value)
}

// UnmarshalSia implements encoding.SiaUnmarshaler.
func (i *ValuedInput) UnmarshalSia(r io.Reader) error {
	return encoding.NewDecoder(r, encoding.DefaultAllocLimit).DecodeAll(&i.SiacoinInput, &i.Value)
}

// A BlockReward is a timelocked output awarded to the miner of a block.
type BlockReward struct {
	UnspentOutput
	Timelock types.BlockHeight
}

// MarshalSia implements encoding.SiaMarshaler.
func (br BlockReward) MarshalSia(w io.Writer) error {
	return encoding.NewEncoder(w).EncodeAll(br.UnspentOutput, br.Timelock)
}

// UnmarshalSia implements encoding.SiaUnmarshaler.
func (br *BlockReward) UnmarshalSia(r io.Reader) error {
	return encoding.NewDecoder(r, encoding.DefaultAllocLimit).DecodeAll(&br.UnspentOutput, &br.Timelock)
}

// A FileContract is an initial or revised file contract.
type FileContract struct {
	types.FileContract
	UnlockConditions types.UnlockConditions
	ID               types.FileContractID
}

// MarshalSia implements encoding.SiaMarshaler.
func (fc FileContract) MarshalSia(w io.Writer) error {
	return encoding.NewEncoder(w).EncodeAll(fc.FileContract, fc.UnlockConditions, fc.ID)
}

// UnmarshalSia implements encoding.SiaUnmarshaler.
func (fc *FileContract) UnmarshalSia(r io.Reader) error {
	return encoding.NewDecoder(r, encoding.DefaultAllocLimit).DecodeAll(&fc.FileContract, &fc.UnlockConditions, &fc.ID)
}

// A LimboTransaction is a transaction that has been broadcast, but has not
// appeared in a block.
type LimboTransaction struct {
	types.Transaction
	LimboSince time.Time
}

// MarshalSia implements encoding.SiaMarshaler.
func (txn LimboTransaction) MarshalSia(w io.Writer) error {
	since := txn.LimboSince.Unix()
	return encoding.NewEncoder(w).EncodeAll(txn.Transaction, since)
}

// UnmarshalSia implements encoding.SiaUnmarshaler.
func (txn *LimboTransaction) UnmarshalSia(r io.Reader) error {
	var since int64
	err := encoding.NewDecoder(r, encoding.DefaultAllocLimit).DecodeAll(&txn.Transaction, &since)
	txn.LimboSince = time.Unix(since, 0)
	return err
}

// CalculateLimboOutputs returns the outputs the owner would control if all
// transactions in limbo were applied.
func CalculateLimboOutputs(owner AddressOwner, limbo []LimboTransaction, outputs []UnspentOutput) []UnspentOutput {
	newOutputs := append([]UnspentOutput(nil), outputs...)
	// first add all newly-created outputs, then delete all spent outputs; this
	// way, the ordering of the limbo transactions (e.g. if one txn creates an
	// output spent by another txn) is irrelevant
	for _, txn := range limbo {
		for i, o := range txn.SiacoinOutputs {
			if owner.OwnsAddress(o.UnlockHash) {
				newOutputs = append(newOutputs, UnspentOutput{
					SiacoinOutput: o,
					ID:            txn.SiacoinOutputID(uint64(i)),
				})
			}
		}
	}
	for _, txn := range limbo {
		for _, o := range txn.SiacoinInputs {
			if owner.OwnsAddress(CalculateUnlockHash(o.UnlockConditions)) {
				for j := range newOutputs {
					if newOutputs[j].ID == o.ParentID {
						newOutputs = append(newOutputs[:j], newOutputs[j+1:]...)
						break
					}
				}
			}
		}
	}
	return newOutputs
}
