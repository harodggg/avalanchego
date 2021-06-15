package proposervm

// ProposerBlock is a decorator for a snowman.Block, created to handle block headers introduced with snowman++
// ProposerBlock is made up of a ProposerBlockHeader, carrying all the new fields introduced with snowman++, and
// a core block, which is a snowman.Block.
// ProposerBlock serialization is a two step process: the header is serialized at proposervm level, while core block
// serialization is deferred to the ChainVM wrapped into proposervm.VM. The structure marshallingProposerBLock encapsulates
// the serialization logic

import (
	"crypto"
	cryptorand "crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/utils/hashing"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/vms/components/missing"
)

const (
	BlkSubmissionTolerance = 10 * time.Second
	BlkSubmissionWinLength = 2 * time.Second
	proBlkVersion          = 0
)

var (
	ErrInnerBlockNotOracle = errors.New("core snowman block does not implement snowman.OracleBlock")
	ErrProBlkNotFound      = errors.New("proposer block not found")
	ErrProBlkBadTimestamp  = errors.New("proposer block timestamp outside tolerance window")
	ErrInvalidTLSKey       = errors.New("invalid validator signing key")
	ErrInvalidNodeID       = errors.New("could not retrieve nodeID from proposer block certificate")
	ErrInvalidSignature    = errors.New("proposer block signature does not verify")
	ErrProBlkWrongHeight   = errors.New("proposer block has wrong height")
	ErrProBlkFailedParsing = errors.New("could not parse proposer block")
)

type ProposerBlockHeader struct {
	version      uint16
	prntID       ids.ID
	timestamp    int64
	pChainHeight uint64
	valCert      x509.Certificate
	signature    []byte
}

func NewProHeader(prntID ids.ID, unixTime int64, height uint64, cert x509.Certificate) ProposerBlockHeader {
	return ProposerBlockHeader{
		version:      proBlkVersion,
		prntID:       prntID,
		timestamp:    unixTime,
		pChainHeight: height,
		valCert:      cert,
	}
}

type ProposerBlock struct {
	header  ProposerBlockHeader
	coreBlk snowman.Block
	id      ids.ID
	bytes   []byte
	vm      *VM
}

func NewProBlock(vm *VM, hdr ProposerBlockHeader, sb snowman.Block, bytes []byte, signBlk bool) (ProposerBlock, error) {
	res := ProposerBlock{
		header:  hdr,
		coreBlk: sb,
		bytes:   bytes,
		vm:      vm,
	}

	if signBlk { // should be done before calling Bytes()
		if err := res.sign(); err != nil {
			return ProposerBlock{}, err
		}
	}

	if bytes == nil {
		res.bytes = res.Bytes()
	}

	res.id = hashing.ComputeHash256Array(res.bytes)
	return res, nil
}

func (pb *ProposerBlock) sign() error {
	pb.header.signature = nil
	msgHash := hashing.ComputeHash256Array(pb.getBytes())
	signKey, ok := pb.vm.stakingCert.PrivateKey.(crypto.Signer)
	if !ok {
		return ErrInvalidTLSKey
	}

	sig, err := signKey.Sign(cryptorand.Reader, msgHash[:], crypto.SHA256)
	if err != nil {
		return err
	}
	pb.header.signature = sig
	return nil
}

// choices.Decidable interface implementation
func (pb *ProposerBlock) ID() ids.ID {
	return pb.id
}

func (pb *ProposerBlock) Accept() error {
	err := pb.coreBlk.Accept()
	if err == nil {
		// pb parent block should not be needed anymore.
		pb.vm.state.wipeFromCacheProBlk(pb.header.prntID)
	}
	return err
}

func (pb *ProposerBlock) Reject() error {
	// TODO: rejection of ProposerBlock does not imply rejection of coreBlk
	// to refactor upon integration with P-chain
	err := pb.coreBlk.Reject()
	if err == nil {
		pb.vm.state.wipeFromCacheProBlk(pb.id)
	}
	return err
}

func (pb *ProposerBlock) Status() choices.Status {
	return pb.coreBlk.Status()
}

// snowman.Block interface implementation
func (pb *ProposerBlock) Parent() snowman.Block {
	if res, err := pb.vm.state.getProBlock(pb.header.prntID); err == nil {
		return res
	}

	return &missing.Block{BlkID: pb.header.prntID}
}

func (pb *ProposerBlock) Verify() error {
	// validate version
	if pb.header.version != proBlkVersion {
		return fmt.Errorf("codecVersion not matching")
	}

	// validate core block
	if err := pb.coreBlk.Verify(); err != nil {
		return err
	}

	// validate parent
	prntBlk, err := pb.vm.state.getProBlock(pb.header.prntID)
	if err != nil {
		return ErrProBlkNotFound
	}

	// validate height
	if pb.header.pChainHeight < prntBlk.header.pChainHeight {
		return ErrProBlkWrongHeight
	}

	if pb.header.pChainHeight > pb.vm.pChainHeight() {
		return ErrProBlkWrongHeight
	}

	// validate timestamp
	if pb.header.timestamp < prntBlk.header.timestamp {
		return ErrProBlkBadTimestamp
	}

	nodeID, err := ids.ToShortID(hashing.PubkeyBytesToAddress(pb.header.valCert.Raw))
	if err != nil {
		return ErrInvalidNodeID
	}

	blkWinDelay := pb.vm.BlkSubmissionDelay(pb.header.pChainHeight, nodeID)
	blkWinStart := time.Unix(prntBlk.header.timestamp, 0).Add(blkWinDelay)
	if time.Unix(pb.header.timestamp, 0).Before(blkWinStart) {
		return ErrProBlkBadTimestamp
	}

	if time.Unix(pb.header.timestamp, 0).After(pb.vm.now().Add(BlkSubmissionTolerance)) {
		return ErrProBlkBadTimestamp
	}

	// validate signature.
	blkSignature := make([]byte, len(pb.header.signature))
	copy(blkSignature, pb.header.signature)
	pb.header.signature = make([]byte, 0)

	blkBytes := make([]byte, len(pb.bytes))
	copy(blkBytes, pb.bytes)
	pb.bytes = make([]byte, 0)

	signedBytes := pb.getBytes()
	pb.header.signature = blkSignature
	pb.bytes = blkBytes

	if err = pb.header.valCert.CheckSignature(pb.header.valCert.SignatureAlgorithm,
		signedBytes, pb.header.signature); err != nil {
		return ErrInvalidSignature
	}

	return nil
}

func (pb *ProposerBlock) getBytes() []byte {
	var mPb marshallingProposerBLock
	mPb.ProposerBlockHeader = pb.header
	mPb.wrpdBytes = pb.coreBlk.Bytes()

	res, err := mPb.marshal()
	if err != nil {
		res = make([]byte, 0)
	}
	return res
}

func (pb *ProposerBlock) Bytes() []byte {
	if pb.bytes == nil {
		pb.bytes = pb.getBytes()
	}
	return pb.bytes
}

func (pb *ProposerBlock) Height() uint64 {
	return pb.coreBlk.Height()
}

func (pb *ProposerBlock) Timestamp() time.Time {
	return pb.coreBlk.Timestamp()
}

// snowman.OracleBlock interface implementation
func (pb *ProposerBlock) Options() ([2]snowman.Block, error) {
	if oracleBlk, ok := pb.coreBlk.(snowman.OracleBlock); ok {
		return oracleBlk.Options()
	}

	return [2]snowman.Block{}, ErrInnerBlockNotOracle
}

type marshallingProposerBLock struct {
	ProposerBlockHeader
	wrpdBytes []byte
}

func (mPb *marshallingProposerBLock) marshal() ([]byte, error) {
	p := wrappers.Packer{
		MaxSize: 1 << 18,
		Bytes:   make([]byte, 0, 128),
	}
	if p.PackShort(mPb.version); p.Errored() {
		return nil, ErrProBlkFailedParsing
	}

	if p.PackBytes(mPb.prntID[:]); p.Errored() {
		return nil, ErrProBlkFailedParsing
	}

	if p.PackLong(uint64(mPb.timestamp)); p.Errored() {
		return nil, ErrProBlkFailedParsing
	}

	if p.PackLong(mPb.pChainHeight); p.Errored() {
		return nil, ErrProBlkFailedParsing
	}

	if p.PackX509Certificate(&mPb.valCert); p.Errored() {
		return nil, ErrProBlkFailedParsing
	}

	if p.PackBytes(mPb.signature); p.Errored() {
		return nil, ErrProBlkFailedParsing
	}

	if p.PackBytes(mPb.wrpdBytes); p.Errored() {
		return nil, ErrProBlkFailedParsing
	}

	return p.Bytes, nil
}

func (mPb *marshallingProposerBLock) unmarshal(b []byte) error {
	p := wrappers.Packer{
		Bytes: b,
	}

	if mPb.version = p.UnpackShort(); p.Errored() {
		return ErrProBlkFailedParsing
	}

	prntIDBytes := p.UnpackBytes()
	switch {
	case p.Errored():
		return ErrProBlkFailedParsing
	case len(prntIDBytes) != len(mPb.prntID):
		return ErrProBlkFailedParsing
	default:
		copy(mPb.prntID[:], prntIDBytes)
	}

	if mPb.timestamp = int64(p.UnpackLong()); p.Errored() {
		return ErrProBlkFailedParsing
	}

	if mPb.pChainHeight = p.UnpackLong(); p.Errored() {
		return ErrProBlkFailedParsing
	}

	pValCert := p.UnpackX509Certificate()
	if p.Errored() {
		return ErrProBlkFailedParsing
	}
	if pValCert != nil {
		mPb.valCert = *pValCert
	} else {
		mPb.valCert = x509.Certificate{} // special case: genesis has empty certificate
	}

	if mPb.signature = p.UnpackBytes(); p.Errored() {
		return ErrProBlkFailedParsing
	}

	if mPb.wrpdBytes = p.UnpackBytes(); p.Errored() {
		return ErrProBlkFailedParsing
	}

	return nil
}