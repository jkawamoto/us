package proto

import (
	"io"
	"net"
	"time"
	"unsafe"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/lukechampine/us/hostdb"
	"github.com/pkg/errors"
)

// sectorBuffer is used to efficiently read sector data from the host.
// Normally we would decode into a [][]byte, but this would cause the encoding
// package to allocate a new slice each time. Furthermore, we only ever
// request one sector, so a [][]byte is unnecessary. Implementing a custom
// SiaUnmarshaler addresses both of these problems.
type sectorBuffer []byte

func (sb *sectorBuffer) UnmarshalSia(r io.Reader) error {
	if cap(*sb) != SectorSize {
		*sb = make([]byte, SectorSize)
	}
	buf := *sb // for convenience

	// first 24 bytes are length prefixes
	_, err := io.ReadFull(r, buf[:24])
	if err != nil {
		return err
	}
	totalSize := encoding.DecUint64(buf[0:8])
	numSectors := encoding.DecUint64(buf[8:16])
	payloadSize := encoding.DecUint64(buf[16:24])
	if totalSize > SectorSize+16 {
		return errors.New("reported payload size is larger than SectorSize")
	} else if numSectors != 1 {
		return errors.New("wrong number of sectors")
	} else if payloadSize > SectorSize {
		return errors.New("reported sector data is larger than SectorSize")
	}

	// read sector data
	*sb = buf[:payloadSize]
	_, err = io.ReadFull(r, *sb)
	return err
}

// A Downloader retrieves sectors by calling the download RPC on a host. It
// updates the corresponding contract after each iteration of the download
// protocol.
type Downloader struct {
	host     hostdb.ScannedHost
	contract ContractEditor
	sector   sectorBuffer // reuse buffer for each download
	conn     net.Conn
}

// HostKey returns the public key of the host being downloaded from.
func (d *Downloader) HostKey() hostdb.HostPublicKey {
	return d.host.PublicKey
}

// Close cleanly terminates the download loop with the host and closes the
// connection.
func (d *Downloader) Close() error {
	return terminateRPC(d.conn, d.host)
}

// Sector retrieves the sector with the specified Merkle root, and revises the
// underlying contract to pay the host appropriately. Sector verifies the
// integrity of the retrieved data by comparing its computed Merkle root to
// root. The returned sector is only valid until the next call to Sector or
// PartialSector.
func (d *Downloader) Sector(root crypto.Hash) (*[SectorSize]byte, error) {
	sectorSlice, err := d.PartialSector(root, 0, SectorSize)
	if err != nil {
		return nil, err
	}
	sector := (*[SectorSize]byte)(unsafe.Pointer(&sectorSlice[0]))
	if SectorMerkleRoot(sector) != root {
		return nil, errors.New("host sent invalid sector data")
	}
	return sector, nil
}

// PartialSector retrieves the slice of sector data uniquely identified by
// root, offset, and length, and revises the underlying contract to pay the
// host proportionally to the data retrieved. The returned slice is only valid
// until the next call to Sector.
//
// Unlike Sector, the integrity of the data cannot be verified by computing
// its Merkle root. Callers must implement a different means of integrity-
// checking, such as comparing against a known checksum.
func (d *Downloader) PartialSector(root crypto.Hash, offset, length uint32) ([]byte, error) {
	data, err := d.partialSector(root, offset, length)
	if isHostDisconnect(err) {
		// try reconnecting
		d.conn.Close()
		d.conn, err = initiateRPC(d.host.NetAddress, modules.RPCDownload, d.contract)
		if err != nil {
			return nil, err
		}
		data, err = d.partialSector(root, offset, length)
	}
	return data, err
}

func (d *Downloader) partialSector(root crypto.Hash, offset, length uint32) ([]byte, error) {
	extendDeadline(d.conn, modules.NegotiateDownloadTime)
	defer extendDeadline(d.conn, time.Hour) // reset deadline when finished

	// sanity check for offset and length
	if offset+length > SectorSize {
		return nil, errors.New("invalid sector range")
	}

	// initiate download, updating host settings
	if err := startRevision(d.conn, &d.host); err != nil {
		return nil, err
	}

	// calculate price
	txn := d.contract.Transaction()
	sectorPrice := d.host.DownloadBandwidthPrice.Mul64(SectorSize)
	if txn.RenterFunds().Cmp(sectorPrice) < 0 {
		return nil, errors.New("contract has insufficient funds to support download")
	}

	// send download action
	err := encoding.WriteObject(d.conn, []modules.DownloadAction{{
		MerkleRoot: root,
		Offset:     uint64(offset),
		Length:     uint64(length),
	}})
	if err != nil {
		return nil, errors.Wrap(err, "could not send revision action")
	}

	// create the download revision
	rev := newDownloadRevision(txn.CurrentRevision(), sectorPrice)

	// send the revision to the host for approval
	signedTxn, err := negotiateRevision(d.conn, rev, txn.RenterKey)
	if err == modules.ErrStopResponse {
		// if host gracefully closed, ignore the error. The next download
		// attempt will return an error that satisfies IsHostDisconnect.
	} else if err != nil {
		d.conn.Close()
		return nil, err
	}

	// update contract revision
	err = d.contract.SyncWithHost(signedTxn.FileContractRevisions[0], signedTxn.TransactionSignatures)
	if err != nil {
		return nil, errors.Wrap(err, "could not update contract transaction")
	}

	// read sector data, completing one iteration of the download loop
	if err := d.sector.UnmarshalSia(d.conn); err != nil {
		d.conn.Close()
		return nil, err
	} else if uint32(len(d.sector)) != length {
		d.conn.Close()
		return nil, errors.New("host sent incomplete sector data")
	}

	return d.sector, nil
}

// NewDownloader initiates the download request loop with a host, and returns a
// Downloader.
func NewDownloader(host hostdb.ScannedHost, contract ContractEditor) (*Downloader, error) {
	conn, err := initiateRPC(host.NetAddress, modules.RPCDownload, contract)
	if err != nil {
		return nil, err
	}
	return &Downloader{
		contract: contract,
		host:     host,
		conn:     conn,
	}, nil
}
