package format

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Castagnoli table for hardware-accelerated CRC32-C
var CastagnoliTable = crc32.MakeTable(crc32.Castagnoli)

const (
	FileHeaderSize   = 32
	RecordHeaderSize = 32

	MagicRecord = uint32(0x52454330) // "REC0"
	MagicCommit = uint32(0x434F4D54) // "COMT"

	OpPut    uint8 = 0x01
	OpDelete uint8 = 0x02
	OpCommit uint8 = 0x03

	FlagBatchOp uint8 = 0x01

	MaxKeySize   = 1024
	MaxValueSize = 1048576 // 1 MB
)

var (
	MagicFileHeader = [8]byte{'K', 'V', 'L', 'O', 'G', '_', 'V', '1'}

	ErrCorruptHeader = errors.New("corrupt file header")
	ErrCorruptRecord = errors.New("corrupt record checksum or magic")
	ErrTornWrite     = errors.New("torn write detected at tail")
	ErrKeyTooLarge   = errors.New("key exceeds maximum size of 1024 bytes")
	ErrValTooLarge   = errors.New("value exceeds maximum size of 1 MB")
)

// FileHeader represents the fixed 32-byte header at offset 0
type FileHeader struct {
	Magic     [8]byte
	Version   uint16
	Flags     uint16
	Reserved  uint32
	Timestamp uint64
	Checksum  uint32
}

// EncodeFileHeader serializes the 32-byte file header
func EncodeFileHeader(timestamp uint64) []byte {
	buf := make([]byte, FileHeaderSize)
	copy(buf[0:8], MagicFileHeader[:])
	binary.LittleEndian.PutUint16(buf[8:10], 1) // Version 1
	binary.LittleEndian.PutUint16(buf[10:12], 0) // Flags
	binary.LittleEndian.PutUint32(buf[12:16], 0) // Reserved
	binary.LittleEndian.PutUint64(buf[16:24], timestamp)

	crc := crc32.Checksum(buf[0:24], CastagnoliTable)
	binary.LittleEndian.PutUint32(buf[24:28], crc)
	binary.LittleEndian.PutUint32(buf[28:32], 0) // Padding
	return buf
}

// ValidateFileHeader verifies the 32-byte file header
func ValidateFileHeader(buf []byte) (*FileHeader, error) {
	if len(buf) < FileHeaderSize {
		return nil, ErrCorruptHeader
	}
	var h FileHeader
	copy(h.Magic[:], buf[0:8])
	if h.Magic != MagicFileHeader {
		return nil, ErrCorruptHeader
	}
	h.Version = binary.LittleEndian.Uint16(buf[8:10])
	h.Flags = binary.LittleEndian.Uint16(buf[10:12])
	h.Reserved = binary.LittleEndian.Uint32(buf[12:16])
	h.Timestamp = binary.LittleEndian.Uint64(buf[16:24])
	h.Checksum = binary.LittleEndian.Uint32(buf[24:28])

	expectedCRC := crc32.Checksum(buf[0:24], CastagnoliTable)
	if h.Checksum != expectedCRC {
		return nil, ErrCorruptHeader
	}
	return &h, nil
}

// RecordHeader represents the 32-byte header preceding key/value payloads
type RecordHeader struct {
	Magic      uint32
	TotalLen   uint32
	Checksum   uint32
	OpType     uint8
	Flags      uint8
	KeyLen     uint16
	TxID       uint64
	ValLen     uint32
	BatchCount uint32
}

// EncodeRecord serializes a standard PUT or DELETE record
func EncodeRecord(opType uint8, flags uint8, txID uint64, key, val []byte) ([]byte, error) {
	if len(key) > MaxKeySize {
		return nil, ErrKeyTooLarge
	}
	if len(val) > MaxValueSize {
		return nil, ErrValTooLarge
	}

	totalLen := uint32(RecordHeaderSize + len(key) + len(val))
	buf := make([]byte, totalLen)

	binary.LittleEndian.PutUint32(buf[0:4], MagicRecord)
	binary.LittleEndian.PutUint32(buf[4:8], totalLen)
	// Checksum at [8:12] computed after filling body
	buf[12] = opType
	buf[13] = flags
	binary.LittleEndian.PutUint16(buf[14:16], uint16(len(key)))
	binary.LittleEndian.PutUint64(buf[16:24], txID)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(len(val)))
	binary.LittleEndian.PutUint32(buf[28:32], 0) // BatchCount unused for regular ops

	copy(buf[32:32+len(key)], key)
	copy(buf[32+len(key):], val)

	crc := crc32.Checksum(buf[12:], CastagnoliTable)
	binary.LittleEndian.PutUint32(buf[8:12], crc)

	return buf, nil
}

// EncodeCommitMarker serializes a 32-byte COMMIT record sealing a batch
func EncodeCommitMarker(txID uint64, batchCount uint32) []byte {
	buf := make([]byte, RecordHeaderSize)
	binary.LittleEndian.PutUint32(buf[0:4], MagicCommit)
	binary.LittleEndian.PutUint32(buf[4:8], RecordHeaderSize)
	buf[12] = OpCommit
	buf[13] = FlagBatchOp
	binary.LittleEndian.PutUint16(buf[14:16], 0) // KeyLen = 0
	binary.LittleEndian.PutUint64(buf[16:24], txID)
	binary.LittleEndian.PutUint32(buf[24:28], 0) // ValLen = 0
	binary.LittleEndian.PutUint32(buf[28:32], batchCount)

	crc := crc32.Checksum(buf[12:], CastagnoliTable)
	binary.LittleEndian.PutUint32(buf[8:12], crc)

	return buf
}

// DecodeChecksum calculates the Castagnoli CRC32-C checksum over a byte slice
func DecodeChecksum(data []byte) uint32 {
	return crc32.Checksum(data, CastagnoliTable)
}

func DecodeRecordHeader(buf []byte) (*RecordHeader, error) {
	if len(buf) < RecordHeaderSize {
		return nil, ErrTornWrite
	}
	magic := binary.LittleEndian.Uint32(buf[0:4])
	if magic != MagicRecord && magic != MagicCommit {
		return nil, ErrCorruptRecord
	}
	rh := &RecordHeader{
		Magic:      magic,
		TotalLen:   binary.LittleEndian.Uint32(buf[4:8]),
		Checksum:   binary.LittleEndian.Uint32(buf[8:12]),
		OpType:     buf[12],
		Flags:      buf[13],
		KeyLen:     binary.LittleEndian.Uint16(buf[14:16]),
		TxID:       binary.LittleEndian.Uint64(buf[16:24]),
		ValLen:     binary.LittleEndian.Uint32(buf[24:28]),
		BatchCount: binary.LittleEndian.Uint32(buf[28:32]),
	}
	if rh.TotalLen < RecordHeaderSize {
		return nil, ErrCorruptRecord
	}
	return rh, nil
}
