package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"logicrush/src/engine"
	"logicrush/src/format"
)

func TestSinglePutGetDelete(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := engine.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	key := []byte("user:100")
	val := []byte("val_alice_active")

	if err := db.Put(key, val); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	got, err := db.Get(key)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if string(got) != string(val) {
		t.Fatalf("Get expected %s, got %s", val, got)
	}

	if err := db.Delete(key); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	_, err = db.Get(key)
	if err != engine.ErrNotFound {
		t.Fatalf("Expected ErrNotFound after delete, got %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

// Scenario B5: A batch that puts key X and deletes key X in the same batch
func TestBatchIntraOrdering(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "batch_order.db")

	db, err := engine.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ops := []engine.BatchOp{
		{Type: format.OpPut, Key: []byte("order_key"), Value: []byte("val_one")},
		{Type: format.OpDelete, Key: []byte("order_key")},
	}

	if err := db.Batch(ops); err != nil {
		t.Fatalf("Batch failed: %v", err)
	}

	_, err = db.Get([]byte("order_key"))
	if err != engine.ErrNotFound {
		t.Fatalf("Expected key to be deleted by intra-batch order, got: %v", err)
	}

	db.Close()

	// Reopen and verify it stays gone after recovery
	db2, err := engine.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer db2.Close()

	_, err = db2.Get([]byte("order_key"))
	if err != engine.ErrNotFound {
		t.Fatalf("Expected key to remain deleted after reopen, got: %v", err)
	}
}

// Scenario F4: Database file truncated by 1 byte at the end by hand
func TestTornTailTruncation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "torn.db")

	db, err := engine.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	db.Put([]byte("valid_k1"), []byte("valid_v1"))
	db.Put([]byte("valid_k2"), []byte("valid_v2"))
	db.Close()

	// Truncate file by 1 byte by hand
	stat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if err := os.Truncate(dbPath, stat.Size()-1); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	// Recovery must succeed cleanly
	db2, err := engine.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("Reopen on torn tail failed: %v", err)
	}
	defer db2.Close()

	v1, err := db2.Get([]byte("valid_k1"))
	if err != nil || string(v1) != "valid_v1" {
		t.Fatalf("Valid earlier record missing or corrupted: %v", err)
	}

	if err := db2.Check(); err != nil {
		t.Fatalf("Check failed on recovered database: %v", err)
	}
}

// Scenario F3: Benchmark recovery of 1,000,000 keys under 2 seconds
func TestRecoveryOneMillionKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1M benchmark in short mode")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "million.db")

	db, err := engine.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	t.Log("Writing 1,000,000 keys in batches...")
	batchSize := 5000
	for i := 0; i < 1_000_000; i += batchSize {
		ops := make([]engine.BatchOp, batchSize)
		for j := 0; j < batchSize; j++ {
			k := fmt.Sprintf("k%07d", i+j)
			v := fmt.Sprintf("v%07d", i+j)
			ops[j] = engine.BatchOp{
				Type:  format.OpPut,
				Key:   []byte(k),
				Value: []byte(v),
			}
		}
		if err := db.Batch(ops); err != nil {
			t.Fatalf("Batch failed at %d: %v", i, err)
		}
	}
	db.Close()

	t.Log("Starting benchmark: open() and recover 1,000,000 keys...")
	start := time.Now()
	dbRecovered, err := engine.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("Open/Recovery failed: %v", err)
	}
	defer dbRecovered.Close()
	elapsed := time.Since(start)

	t.Logf("Replayed 1,000,000 keys in %v (Target: < 2.0s)", elapsed)
	if elapsed > 2*time.Second {
		t.Fatalf("Recovery exceeded 2.0s limit: %v", elapsed)
	}
	if dbRecovered.KeyCount() != 1_000_000 {
		t.Fatalf("Expected 1,000,000 keys in index, got %d", dbRecovered.KeyCount())
	}
}
