package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"logicrush/src/format"
)

var (
	ErrDiskFull  = errors.New("database file size limit reached")
	ErrNotFound  = errors.New("key not found")
	ErrClosed    = errors.New("database is closed")
	ErrBatchEmpty = errors.New("batch cannot be empty")
)

// BatchOp represents an operation within an atomic batch
type BatchOp struct {
	Type  uint8  // format.OpPut or format.OpDelete
	Key   []byte
	Value []byte
}

// KVPair represents a key-value pair for Dump
type KVPair struct {
	Key   string
	Value []byte
}

// DB represents the embedded crash-safe key-value database
type DB struct {
	mu          sync.RWMutex
	file        *os.File
	path        string
	maxBytes    int64
	currentSize int64
	txID        uint64
	index       *Index
	closed      bool
}

// Open opens or creates the database file and runs crash recovery
func Open(path string, maxBytes int64) (*DB, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, fmt.Errorf("failed to open database file: %w", err)
	}

	index, maxTxID, validOffset, err := Recover(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("recovery failed: %w", err)
	}

	db := &DB{
		file:        f,
		path:        path,
		maxBytes:    maxBytes,
		currentSize: validOffset,
		txID:        maxTxID,
		index:       index,
	}

	return db, nil
}

// Put writes or overwrites a key-value pair synchronously
func (db *DB) Put(key, val []byte) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return ErrClosed
	}

	recLen := int64(format.RecordHeaderSize + len(key) + len(val))
	if db.maxBytes > 0 && db.currentSize+recLen > db.maxBytes {
		return ErrDiskFull
	}

	db.txID++
	buf, err := format.EncodeRecord(format.OpPut, 0, db.txID, key, val)
	if err != nil {
		return err
	}

	// 1. Write record payload
	if _, err := db.file.WriteAt(buf, db.currentSize); err != nil {
		return err
	}

	// 2. Barrier: synchronously flush to non-volatile disk
	if err := db.file.Sync(); err != nil {
		return err
	}

	valOffset := db.currentSize + int64(format.RecordHeaderSize+len(key))
	db.currentSize += int64(len(buf))

	// 3. Update in-memory index
	db.index.Put(string(key), IndexEntry{Offset: valOffset, Length: uint32(len(val))})
	return nil
}

// Get retrieves a key's value
func (db *DB) Get(key []byte) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.closed {
		return nil, ErrClosed
	}

	entry, found := db.index.Get(string(key))
	if !found {
		return nil, ErrNotFound
	}

	val := make([]byte, entry.Length)
	if entry.Length > 0 {
		if _, err := db.file.ReadAt(val, entry.Offset); err != nil {
			return nil, err
		}
	}
	return val, nil
}

// Delete marks a key as deleted using an append-only tombstone
func (db *DB) Delete(key []byte) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return ErrClosed
	}

	recLen := int64(format.RecordHeaderSize + len(key))
	if db.maxBytes > 0 && db.currentSize+recLen > db.maxBytes {
		return ErrDiskFull
	}

	db.txID++
	buf, err := format.EncodeRecord(format.OpDelete, 0, db.txID, key, nil)
	if err != nil {
		return err
	}

	if _, err := db.file.WriteAt(buf, db.currentSize); err != nil {
		return err
	}

	if err := db.file.Sync(); err != nil {
		return err
	}

	db.currentSize += int64(len(buf))
	db.index.Delete(string(key))
	return nil
}

// Batch applies a series of put/delete operations atomically
func (db *DB) Batch(ops []BatchOp) error {
	if len(ops) == 0 {
		return ErrBatchEmpty
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return ErrClosed
	}

	// 1. Calculate total bytes needed and pre-check maxBytes (Scenario E2)
	var totalRecordsBytes int64
	for _, op := range ops {
		totalRecordsBytes += int64(format.RecordHeaderSize + len(op.Key) + len(op.Value))
	}
	totalBatchBytes := totalRecordsBytes + int64(format.RecordHeaderSize) // includes 32B commit marker

	if db.maxBytes > 0 && db.currentSize+totalBatchBytes > db.maxBytes {
		return ErrDiskFull
	}

	db.txID++
	txID := db.txID

	// 2. Assemble batch records buffer
	var recordsBuf bytes.Buffer
	recordsBuf.Grow(int(totalRecordsBytes))

	type stagedOp struct {
		opType uint8
		key    string
		offset int64
		length uint32
	}
	staged := make([]stagedOp, 0, len(ops))

	currentRecordOffset := db.currentSize
	for _, op := range ops {
		buf, err := format.EncodeRecord(op.Type, format.FlagBatchOp, txID, op.Key, op.Value)
		if err != nil {
			return err
		}
		recordsBuf.Write(buf)

		valOffset := currentRecordOffset + int64(format.RecordHeaderSize+len(op.Key))
		staged = append(staged, stagedOp{
			opType: op.Type,
			key:    string(op.Key),
			offset: valOffset,
			length: uint32(len(op.Value)),
		})
		currentRecordOffset += int64(len(buf))
	}

	// 3. Append all batch records
	if _, err := db.file.WriteAt(recordsBuf.Bytes(), db.currentSize); err != nil {
		return err
	}

	// 4. Physical Disk Barrier 1: ensure records are physically on non-volatile media
	if err := db.file.Sync(); err != nil {
		return err
	}

	// 5. Append Commit Marker
	commitBuf := format.EncodeCommitMarker(txID, uint32(len(ops)))
	if _, err := db.file.WriteAt(commitBuf, currentRecordOffset); err != nil {
		return err
	}

	// 6. Physical Disk Barrier 2: ensure commit marker is physically on non-volatile media
	if err := db.file.Sync(); err != nil {
		return err
	}

	db.currentSize += totalBatchBytes

	// 7. Commit to in-memory index
	for _, op := range staged {
		if op.opType == format.OpPut {
			db.index.Put(op.key, IndexEntry{Offset: op.offset, Length: op.length})
		} else if op.opType == format.OpDelete {
			db.index.Delete(op.key)
		}
	}

	return nil
}

// Dump returns all keys and values sorted lexicographically by key
func (db *DB) Dump() ([]KVPair, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.closed {
		return nil, ErrClosed
	}

	keys := db.index.Keys()
	sort.Strings(keys)

	pairs := make([]KVPair, len(keys))
	for i, k := range keys {
		entry, found := db.index.Get(k)
		if !found {
			continue
		}
		val := make([]byte, entry.Length)
		if entry.Length > 0 {
			if _, err := db.file.ReadAt(val, entry.Offset); err != nil {
				return nil, err
			}
		}
		pairs[i] = KVPair{Key: k, Value: val}
	}
	return pairs, nil
}

// Check scans the entire file and verifies all headers and checksums
func (db *DB) Check() error {
	db.mu.RLock()
	defer db.mu.RUnlock()

	stat, err := db.file.Stat()
	if err != nil {
		return err
	}
	fileSize := stat.Size()
	if fileSize < format.FileHeaderSize {
		return errors.New("file too short to contain valid header")
	}

	headerBuf := make([]byte, format.FileHeaderSize)
	if _, err := db.file.ReadAt(headerBuf, 0); err != nil {
		return err
	}
	if _, err := format.ValidateFileHeader(headerBuf); err != nil {
		return err
	}

	cursor := int64(format.FileHeaderSize)
	for cursor < fileSize {
		if fileSize-cursor < format.RecordHeaderSize {
			return fmt.Errorf("torn record header at offset %d", cursor)
		}
		hBuf := make([]byte, format.RecordHeaderSize)
		if _, err := db.file.ReadAt(hBuf, cursor); err != nil {
			return err
		}
		rh, err := format.DecodeRecordHeader(hBuf)
		if err != nil {
			return fmt.Errorf("corrupt header at offset %d: %w", cursor, err)
		}
		if cursor+int64(rh.TotalLen) > fileSize {
			return fmt.Errorf("truncated record payload at offset %d", cursor)
		}
		fullRec := make([]byte, rh.TotalLen)
		if _, err := db.file.ReadAt(fullRec, cursor); err != nil {
			return err
		}
		chk := format.DecodeChecksum(fullRec[12:])
		if chk != rh.Checksum {
			return fmt.Errorf("CRC mismatch at offset %d", cursor)
		}
		cursor += int64(rh.TotalLen)
	}
	return nil
}

// KeyCount returns the total number of live keys
func (db *DB) KeyCount() int {
	return db.index.Count()
}

// Close cleanly flushes and closes the database
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return nil
	}
	db.closed = true
	if err := db.file.Sync(); err != nil {
		db.file.Close()
		return err
	}
	return db.file.Close()
}
