package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Op struct {
	opID     string
	seq      uint64
	isDelete bool
	key      string
	value    string
}

type Batch struct {
	opID string
	seq  uint64
	ops  []Op
}

// Oracle maintains authoritative state of promised (ACKed) operations
type Oracle struct {
	mu          sync.Mutex
	ackedKeys   map[string]Op
	validValues map[string]map[string]bool
}

func NewOracle() *Oracle {
	return &Oracle{
		ackedKeys:   make(map[string]Op),
		validValues: make(map[string]map[string]bool),
	}
}

func (o *Oracle) RecordSent(ops []Op) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, op := range ops {
		if !op.isDelete {
			if o.validValues[op.key] == nil {
				o.validValues[op.key] = make(map[string]bool)
			}
			o.validValues[op.key][op.value] = true
		}
	}
}

func (o *Oracle) ApplyAck(ops []Op) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, op := range ops {
		if existing, ok := o.ackedKeys[op.key]; !ok || op.seq >= existing.seq {
			o.ackedKeys[op.key] = op
		}
	}
}

func (o *Oracle) Snapshot() (map[string]Op, map[string]map[string]bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	snap := make(map[string]Op, len(o.ackedKeys))
	for k, v := range o.ackedKeys {
		snap[k] = v
	}
	vals := make(map[string]map[string]bool, len(o.validValues))
	for k, vMap := range o.validValues {
		inner := make(map[string]bool, len(vMap))
		for v := range vMap {
			inner[v] = true
		}
		vals[k] = inner
	}
	return snap, vals
}

func main() {
	iterations := flag.Int("iterations", 500, "Number of torture iterations")
	seed := flag.Int64("seed", 12345, "Random seed for reproducibility")
	mode := flag.String("mode", "default", "Torture mode: default, large-values, large-batches, compaction, recovery-crash, disk-full")
	flag.Parse()

	fmt.Printf("=== Starting KVDB Torture Suite ===\n")
	fmt.Printf("Iterations: %d | Seed: %d | Mode: %s\n", *iterations, *seed, *mode)

	rng := rand.New(rand.NewSource(*seed))
	oracle := NewOracle()
	dbDir, err := os.MkdirTemp("", "kvdb_torture_*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dbDir)

	dbPath := filepath.Join(dbDir, "authoritative.db")
	kvdbBin := "./kvdb.exe"
	if _, err := os.Stat(kvdbBin); os.IsNotExist(err) {
		kvdbBin = "./kvdb"
	}

	passCount := 0
	startTime := time.Now()
	var globalSeq uint64

	for iter := 1; iter <= *iterations; iter++ {
		iterSeed := rng.Int63()
		err := runIteration(iter, iterSeed, *mode, kvdbBin, dbPath, oracle, &globalSeq)
		if err != nil {
			fmt.Printf("\n[FAILURE] Invariant violation at iteration %d (Seed %d): %v\n", iter, iterSeed, err)
			os.Exit(1)
		}
		passCount++
		snap, _ := oracle.Snapshot()
		fmt.Printf("Iteration %3d/%3d passed [Promised Keys: %d]\n", iter, *iterations, len(snap))
	}

	fmt.Printf("\n=== Torture Suite Passed! %d/%d iterations succeeded in %v ===\n", passCount, *iterations, time.Since(startTime))
}

func runIteration(iter int, seed int64, mode string, kvdbBin, dbPath string, oracle *Oracle, globalSeq *uint64) error {
	rng := rand.New(rand.NewSource(seed))

	if mode == "recovery-crash" {
		cmd := exec.Command(kvdbBin, "open", dbPath)
		if err := cmd.Start(); err == nil {
			time.Sleep(time.Duration(rng.Intn(10)) * time.Millisecond)
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return verifyDatabase(kvdbBin, dbPath, oracle, nil)
	}

	var args []string
	if mode == "disk-full" {
		args = []string{"serve", dbPath, "--max-bytes", "50000"}
	} else {
		args = []string{"serve", dbPath}
	}

	cmd := exec.Command(kvdbBin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start serve mode: %w", err)
	}

	var pendingLock sync.Mutex
	pendingBatches := make(map[string]Batch)
	var stdinLock sync.Mutex

	ackReaderDone := make(chan struct{})
	go func() {
		defer close(ackReaderDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(text, "ACK ") {
				ackID := strings.TrimPrefix(text, "ACK ")
				pendingLock.Lock()
				if batch, ok := pendingBatches[ackID]; ok {
					oracle.ApplyAck(batch.ops)
					delete(pendingBatches, ackID)
				}
				pendingLock.Unlock()
			}
		}
	}()

	stopWriters := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	writerFunc := func(writerID int) {
		defer wg.Done()
		localRng := rand.New(rand.NewSource(seed + int64(writerID)*1000))
		for {
			select {
			case <-stopWriters:
				return
			default:
				stdinLock.Lock()
				seq := atomic.AddUint64(globalSeq, 1)
				opID := fmt.Sprintf("w%d_%d", writerID, seq)

				var cmdStr string
				var opsToApply []Op

				if mode == "large-values" {
					valSize := 128*1024 + localRng.Intn(384*1024)
					val := strings.Repeat(fmt.Sprintf("%d", localRng.Intn(10)), valSize)
					key := fmt.Sprintf("lkey_%d", localRng.Intn(30))
					cmdStr = fmt.Sprintf("put %s %s %s\n", opID, key, val)
					opsToApply = []Op{{opID: opID, seq: seq, key: key, value: val}}
				} else if mode == "compaction" && localRng.Intn(8) == 0 {
					cmdStr = fmt.Sprintf("compact %s\n", opID)
				} else {
					choice := localRng.Intn(4)
					if choice == 0 {
						key := fmt.Sprintf("k%d", localRng.Intn(300))
						cmdStr = fmt.Sprintf("del %s %s\n", opID, key)
						opsToApply = []Op{{opID: opID, seq: seq, isDelete: true, key: key}}
					} else if choice == 1 || choice == 2 {
						key := fmt.Sprintf("k%d", localRng.Intn(300))
						val := fmt.Sprintf("v_%d_%d", seq, localRng.Intn(1000))
						cmdStr = fmt.Sprintf("put %s %s %s\n", opID, key, val)
						opsToApply = []Op{{opID: opID, seq: seq, key: key, value: val}}
					} else {
						count := 2 + localRng.Intn(10)
						var sb strings.Builder
						sb.WriteString(fmt.Sprintf("batch %s %d\n", opID, count))
						for b := 0; b < count; b++ {
							bKey := fmt.Sprintf("k%d", localRng.Intn(300))
							if localRng.Intn(3) == 0 {
								sb.WriteString(fmt.Sprintf("del %s\n", bKey))
								opsToApply = append(opsToApply, Op{opID: opID, seq: seq, key: bKey, isDelete: true})
							} else {
								bVal := fmt.Sprintf("bv_%d_%d", seq, b)
								sb.WriteString(fmt.Sprintf("put %s %s\n", bKey, bVal))
								opsToApply = append(opsToApply, Op{opID: opID, seq: seq, key: bKey, value: bVal})
							}
						}
						cmdStr = sb.String()
					}
				}

				if len(opsToApply) > 0 {
					oracle.RecordSent(opsToApply)
					pendingLock.Lock()
					pendingBatches[opID] = Batch{opID: opID, seq: seq, ops: opsToApply}
					pendingLock.Unlock()
				}

				_, _ = io.WriteString(stdin, cmdStr)
				stdinLock.Unlock()

				time.Sleep(time.Duration(localRng.Intn(3)) * time.Millisecond)
			}
		}
	}

	go writerFunc(1)
	go writerFunc(2)

	// Random kill interval
	killDelay := time.Duration(10+rng.Intn(40)) * time.Millisecond
	time.Sleep(killDelay)

	// KILL -9 equivalent
	_ = cmd.Process.Kill()
	close(stopWriters)
	_ = stdin.Close()
	_ = cmd.Wait()
	<-ackReaderDone

	pendingLock.Lock()
	inFlight := make([]Batch, 0, len(pendingBatches))
	for _, b := range pendingBatches {
		inFlight = append(inFlight, b)
	}
	pendingLock.Unlock()

	return verifyDatabase(kvdbBin, dbPath, oracle, inFlight)
}

func verifyDatabase(kvdbBin, dbPath string, oracle *Oracle, inFlight []Batch) error {
	// 1. Run check
	checkCmd := exec.Command(kvdbBin, "check", dbPath)
	var checkOut, checkErr bytes.Buffer
	checkCmd.Stdout = &checkOut
	checkCmd.Stderr = &checkErr
	if err := checkCmd.Run(); err != nil {
		return fmt.Errorf("kvdb check failed: %v | %s", err, checkErr.String())
	}
	if !strings.Contains(checkOut.String(), "OK") {
		return fmt.Errorf("check returned non-OK: %s", checkOut.String())
	}

	// 2. Run dump
	dumpCmd := exec.Command(kvdbBin, "dump", dbPath)
	var dumpOut bytes.Buffer
	dumpCmd.Stdout = &dumpOut
	dumpCmd.Stderr = os.Stderr
	if err := dumpCmd.Run(); err != nil {
		return fmt.Errorf("kvdb dump failed: %v", err)
	}

	actualMap := make(map[string]string)
	scanner := bufio.NewScanner(&dumpOut)
	dumpBuf := make([]byte, 4*1024*1024)
	scanner.Buffer(dumpBuf, 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			actualMap[parts[0]] = parts[1]
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("dump scanner error: %w", err)
	}

	ackedSnap, validVals := oracle.Snapshot()

	// In-flight candidates per key
	inFlightCandidates := make(map[string][]Op)
	inFlightLatest := make(map[string]Op)
	for _, batch := range inFlight {
		for _, op := range batch.ops {
			inFlightCandidates[op.key] = append(inFlightCandidates[op.key], op)
			if existing, ok := inFlightLatest[op.key]; !ok || op.seq > existing.seq {
				inFlightLatest[op.key] = op
			}
		}
	}

	// Invariant Rule 4: No key ever holds garbage or a torn value
	for k, actualV := range actualMap {
		allowedVals, exists := validVals[k]
		if !exists || !allowedVals[actualV] {
			return fmt.Errorf("DATA CORRUPTION: Key %q holds unknown/garbage value %q", k, actualV)
		}
	}

	// Invariant Rule 1: Every ACKed operation must be present (unless superseded by a valid in-flight write)
	for k, ackedOp := range ackedSnap {
		actualV, found := actualMap[k]

		if ackedOp.isDelete {
			if found {
				// If key is present, verify that a later in-flight write resurrected it
				resurrected := false
				if candidates, ok := inFlightCandidates[k]; ok {
					for _, cand := range candidates {
						if !cand.isDelete && cand.seq > ackedOp.seq && actualV == cand.value {
							oracle.ApplyAck([]Op{cand})
							resurrected = true
							break
						}
					}
				}
				if resurrected {
					continue
				}
				return fmt.Errorf("PROMISE BROKEN: Key %q was ACKed deleted, but is still present with value %q", k, actualV)
			}
		} else {
			if !found {
				// If missing, check if a later in-flight delete removed it
				deletedByInflight := false
				if candidates, ok := inFlightCandidates[k]; ok {
					for _, cand := range candidates {
						if cand.isDelete && cand.seq > ackedOp.seq {
							oracle.ApplyAck([]Op{cand})
							deletedByInflight = true
							break
						}
					}
				}
				if deletedByInflight {
					continue
				}
				return fmt.Errorf("PROMISE BROKEN: ACKed key %q is missing from database", k)
			}
			if actualV != ackedOp.value {
				// Check if a later in-flight overwrite took effect
				overwrittenByInflight := false
				if candidates, ok := inFlightCandidates[k]; ok {
					for _, cand := range candidates {
						if !cand.isDelete && cand.seq > ackedOp.seq && actualV == cand.value {
							oracle.ApplyAck([]Op{cand})
							overwrittenByInflight = true
							break
						}
					}
				}
				if overwrittenByInflight {
					continue
				}
				return fmt.Errorf("DATA CORRUPTION: Key %q expected %q, got %q", k, ackedOp.value, actualV)
			}
		}
	}

	// Invariant Rule 3: Every batch is strictly all-or-nothing
	for _, b := range inFlight {
		putsTotal := 0
		putsMatched := 0
		for _, op := range b.ops {
			if !op.isDelete {
				putsTotal++
				if actualV, found := actualMap[op.key]; found && actualV == op.value {
					putsMatched++
				}
			}
		}

		if putsTotal > 0 {
			if putsMatched > 0 && putsMatched < putsTotal {
				return fmt.Errorf("BATCH ATOMICITY VIOLATION: Batch %s had %d of %d PUTs applied", b.opID, putsMatched, putsTotal)
			}
			if putsMatched == putsTotal {
				for _, op := range b.ops {
					if op.isDelete {
						if _, found := actualMap[op.key]; found {
							if ifl, ok := inFlightLatest[op.key]; !ok || ifl.seq <= b.seq {
								return fmt.Errorf("BATCH ATOMICITY VIOLATION: Batch %s applied but deleted key %s still exists", b.opID, op.key)
							}
						}
					}
				}
			}
		}
	}

	return nil
}
