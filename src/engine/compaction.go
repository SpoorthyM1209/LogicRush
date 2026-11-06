package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"logicrush/src/format"
)

// Compact rewrites the database file, purging tombstoned and overwritten records
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return ErrClosed
	}

	tmpPath := db.path + ".compact.tmp"
	tmpFile, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return fmt.Errorf("failed to create compaction temp file: %w", err)
	}

	// 1. Write fresh 32-byte file header
	header := format.EncodeFileHeader(uint64(time.Now().UnixNano()))
	if _, err := tmpFile.WriteAt(header, 0); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}

	keys := db.index.Keys()
	var offset int64 = format.FileHeaderSize
	var newTxID uint64 = 0

	// 2. Write only active, non-deleted records
	for _, k := range keys {
		entry, found := db.index.Get(k)
		if !found {
			continue
		}
		val := make([]byte, entry.Length)
		if entry.Length > 0 {
			if _, err := db.file.ReadAt(val, entry.Offset); err != nil {
				tmpFile.Close()
				os.Remove(tmpPath)
				return err
			}
		}

		newTxID++
		recBuf, err := format.EncodeRecord(format.OpPut, 0, newTxID, []byte(k), val)
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return err
		}

		if _, err := tmpFile.WriteAt(recBuf, offset); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return err
		}
		offset += int64(len(recBuf))
	}

	// 3. Sync temporary file to physical storage
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	tmpFile.Close()

	// 4. Close existing file descriptor before atomic swap
	if err := db.file.Close(); err != nil {
		return err
	}

	// 5. Atomic file replacement
	if err := os.Rename(tmpPath, db.path); err != nil {
		return fmt.Errorf("atomic rename swap failed: %w", err)
	}

	// 6. Parent directory sync for POSIX durability
	parentDir := filepath.Dir(db.path)
	if dirF, err := os.Open(parentDir); err == nil {
		dirF.Sync()
		dirF.Close()
	}

	// 7. Re-open authoritative database file
	newFile, err := os.OpenFile(db.path, os.O_RDWR, 0666)
	if err != nil {
		return err
	}

	newIndex, maxTx, validOff, err := Recover(newFile)
	if err != nil {
		newFile.Close()
		return err
	}

	db.file = newFile
	db.currentSize = validOff
	db.txID = maxTx
	db.index = newIndex

	return nil
}
