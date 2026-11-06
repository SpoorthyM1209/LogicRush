package engine

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"time"

	"logicrush/src/format"
)

type pendingOp struct {
	opType uint8
	key    string
	offset int64
	length uint32
}

// Recover replays the append-only log, repairs torn tails, and rebuilds the index
func Recover(f *os.File) (*Index, uint64, int64, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	fileSize := stat.Size()

	// If file is empty or too short for a header, initialize fresh database
	if fileSize < format.FileHeaderSize {
		header := format.EncodeFileHeader(uint64(time.Now().UnixNano()))
		if _, err := f.WriteAt(header, 0); err != nil {
			return nil, 0, 0, err
		}
		if err := f.Sync(); err != nil {
			return nil, 0, 0, err
		}
		return NewIndex(1024), 0, format.FileHeaderSize, nil
	}

	// Validate File Header
	headerBuf := make([]byte, format.FileHeaderSize)
	if _, err := f.ReadAt(headerBuf, 0); err != nil {
		return nil, 0, 0, err
	}
	if _, err := format.ValidateFileHeader(headerBuf); err != nil {
		return nil, 0, 0, err
	}

	index := NewIndex(1024)
	var maxTxID uint64
	lastValidOffset := int64(format.FileHeaderSize)
	cursor := int64(format.FileHeaderSize)

	var pendingBatch []pendingOp
	var pendingTxID uint64

	// Streaming buffer: 4 MB chunks for sub-2s scan of 1M+ keys
	const bufCap = 4 * 1024 * 1024
	buf := make([]byte, bufCap)
	bufStartOffset := cursor
	bufLen := 0

	for cursor < fileSize {
		// Calculate available bytes in buffer
		offsetInBuf := int(cursor - bufStartOffset)
		avail := bufLen - offsetInBuf

		// If less than a record header remains in buffer, refill buffer from disk
		if avail < format.RecordHeaderSize {
			if offsetInBuf > 0 && avail > 0 {
				copy(buf, buf[offsetInBuf:bufLen])
			}
			bufStartOffset = cursor
			n, err := f.ReadAt(buf[avail:], cursor+int64(avail))
			if err != nil && err != io.EOF {
				break // Read error, truncate at last valid
			}
			bufLen = avail + n
			offsetInBuf = 0
			avail = bufLen
		}

		// Check if at least 32 bytes remain for header
		if fileSize-cursor < format.RecordHeaderSize || avail < format.RecordHeaderSize {
			// Torn header at tail
			break
		}

		headerSlice := buf[offsetInBuf : offsetInBuf+format.RecordHeaderSize]
		magic := binary.LittleEndian.Uint32(headerSlice[0:4])
		totalLen := binary.LittleEndian.Uint32(headerSlice[4:8])
		storedCRC := binary.LittleEndian.Uint32(headerSlice[8:12])
		opType := headerSlice[12]
		flags := headerSlice[13]
		keyLen := binary.LittleEndian.Uint16(headerSlice[14:16])
		txID := binary.LittleEndian.Uint64(headerSlice[16:24])
		valLen := binary.LittleEndian.Uint32(headerSlice[24:28])
		batchCount := binary.LittleEndian.Uint32(headerSlice[28:32])

		// Validate magic bytes and length boundaries
		if magic != format.MagicRecord && magic != format.MagicCommit {
			break // Corrupt magic / torn tail
		}
		if totalLen < format.RecordHeaderSize || cursor+int64(totalLen) > fileSize {
			break // Truncated record payload
		}

		// Ensure full record is in buffer
		if avail < int(totalLen) {
			if int(totalLen) > bufCap {
				// Record larger than 4MB buffer (e.g. 1MB+ value), reallocate
				newBuf := make([]byte, totalLen)
				copy(newBuf, buf[offsetInBuf:bufLen])
				buf = newBuf
			} else {
				copy(buf, buf[offsetInBuf:bufLen])
			}
			bufStartOffset = cursor
			n, err := f.ReadAt(buf[avail:], cursor+int64(avail))
			if err != nil && err != io.EOF {
				break
			}
			bufLen = avail + n
			offsetInBuf = 0
			avail = bufLen
			if avail < int(totalLen) {
				break // Incomplete read of record
			}
		}

		recSlice := buf[offsetInBuf : offsetInBuf+int(totalLen)]
		// Verify Castagnoli CRC32-C
		computedCRC := crc32.Checksum(recSlice[12:], format.CastagnoliTable)
		if computedCRC != storedCRC {
			break // Checksum failure: corrupt sector / torn tail
		}

		if txID > maxTxID {
			maxTxID = txID
		}

		// Process verified record
		if opType == format.OpCommit {
			// Commit marker: verify it seals matching pending batch
			if pendingTxID == txID && len(pendingBatch) == int(batchCount) {
				for _, op := range pendingBatch {
					if op.opType == format.OpPut {
						index.Put(op.key, IndexEntry{Offset: op.offset, Length: op.length})
					} else if op.opType == format.OpDelete {
						index.Delete(op.key)
					}
				}
				pendingBatch = nil
				pendingTxID = 0
				lastValidOffset = cursor + int64(totalLen)
			} else {
				break
			}
		} else if flags&format.FlagBatchOp != 0 {
			// Member of an active batch
			if pendingTxID == 0 {
				pendingTxID = txID
			}
			if pendingTxID == txID {
				keyStart := offsetInBuf + format.RecordHeaderSize
				key := string(buf[keyStart : keyStart+int(keyLen)])
				valOffset := cursor + int64(format.RecordHeaderSize+int(keyLen))
				pendingBatch = append(pendingBatch, pendingOp{
					opType: opType,
					key:    key,
					offset: valOffset,
					length: valLen,
				})
			} else {
				break
			}
		} else {
			// Standalone single operation (auto-committed)
			if pendingBatch != nil {
				break
			}
			keyStart := offsetInBuf + format.RecordHeaderSize
			key := string(buf[keyStart : keyStart+int(keyLen)])
			valOffset := cursor + int64(format.RecordHeaderSize+int(keyLen))
			if opType == format.OpPut {
				index.Put(key, IndexEntry{Offset: valOffset, Length: valLen})
			} else if opType == format.OpDelete {
				index.Delete(key)
			}
			lastValidOffset = cursor + int64(totalLen)
		}

		cursor += int64(totalLen)
	}

	// Truncate torn tail or uncommitted trailing batch operations
	if lastValidOffset < fileSize {
		if err := f.Truncate(lastValidOffset); err != nil {
			return nil, 0, 0, err
		}
		if err := f.Sync(); err != nil {
			return nil, 0, 0, err
		}
	}

	if _, err := f.Seek(lastValidOffset, io.SeekStart); err != nil {
		return nil, 0, 0, err
	}

	return index, maxTxID, lastValidOffset, nil
}
