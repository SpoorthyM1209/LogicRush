# BREAKING.md — Attack & Failure Analysis

This document fulfills **Deliverable 3** of the "Kill It and Prove It" hackathon, critically analyzing the database engine's architecture, crash behavior, known limitations, and rejected designs.

---

## 1. Crash Points Table

| # | Exact Function / Line | Disk State if Killed Right Here | Action Taken by Next `open()` |
| :- | :--- | :--- | :--- |
| 1 | `engine.go:Put` (before `file.Write`) | Zero bytes of the new key/value record have reached disk. | Database remains at previous valid state. Key is absent or retains its old value. |
| 2 | `engine.go:Put` (mid-`file.Write`) | Torn tail: partial 32-byte header or partial payload written to disk. | `Recover()` detects incomplete record or CRC32 mismatch; truncates file back to `last_valid_offset`. |
| 3 | `engine.go:Put` (after `Write`, before `fsync`) | Bytes reside in OS page cache; disk controller may not have flushed them. | If bytes reached non-volatile storage with valid CRC, replayed into index; otherwise truncated. |
| 4 | `engine.go:Put` (after `fsync`, before `stdout.Write`) | Record is completely durable and valid on disk. Caller received no ACK. | `Recover()` replays the record. Key is present. (Legal under invariant Rule 2). |
| 5 | `engine.go:Batch` (after 4 of 10 ops written) | 4 records written with `FLAGS=IS_BATCH`, but no `COMMIT_MARKER`. | `Recover()` places records in temporary buffer; reaches EOF without commit marker; discards all 4. |
| 6 | `engine.go:Batch` (all 10 ops written, before commit marker write) | All 10 batch records written; no commit marker on disk. | `Recover()` reaches EOF; uncommitted batch is dropped. Zero operations visible. |
| 7 | `engine.go:Batch` (mid-write of `COMMIT_MARKER`) | Torn commit marker (< 32 bytes or corrupt CRC). | `Recover()` detects invalid marker at tail; truncates file to start of marker; drops entire batch. |
| 8 | `engine.go:Batch` (commit marker written, before 2nd `fsync`) | Marker in page cache. If disk flushed: all visible. If lost: none visible. Never partial. | `Recover()` applies all 10 or none. |
| 9 | `recovery.go:Recover` (during file scan) | Scan is read-only. Disk state unchanged. | Idempotent. Subsequent `open()` restarts the scan from offset 32. |
| 10| `recovery.go:Recover` (during `ftruncate`) | Truncation may have cut part or all of torn tail. | Idempotent. Subsequent `open()` re-evaluates file size and truncates at `last_valid_offset` again. |
| 11| `compaction.go:Compact` (writing temp file) | Authoritative database untouched. Incomplete `.compact.tmp` exists. | `open()` continues using authoritative database; cleans up `.compact.tmp` if present. |
| 12| `compaction.go:Compact` (mid-`os.Rename`) | Filesystem atomic rename: either old file remains authoritative or new compacted file does. | Recovery succeeds regardless: exactly one file is valid and complete. |
| 13| `engine.go:Close` (during `file.Close`) | All acknowledged writes were previously fsynced. | File is closed cleanly; recovery opens normally. |

---

## 2. The Ordering Argument

Our write and commit protocol enforces strict durability and atomicity through double-barrier synchronization:

$$\text{write}(\text{payloads}) \xrightarrow{\text{barrier 1}} \text{fsync}(\text{payloads}) \xrightarrow{\text{order}} \text{write}(\text{commit\_marker}) \xrightarrow{\text{barrier 2}} \text{fsync}(\text{commit\_marker}) \longrightarrow \text{ACK}$$

1. **Why fsync before writing the commit marker?**
   Modern storage controllers and OS caches reorder writes across sectors. If we issue one write containing both batch records and a commit marker without an intervening barrier, a crash can allow the commit marker to reach flash memory while preceding payload sectors are still volatile or partially written. On restart, a naive parser would see a "commit" marker and read garbage payload sectors. By forcing `fsync(file_fd)` on the payload records *before* writing the commit marker, we guarantee that a commit marker on disk **mathematically implies** that all referenced records are 100% durable.
2. **Why fsync the parent directory during compaction?**
   In POSIX filesystems (ext4, APFS), `rename("db.compact.tmp", "db.log")` updates directory entries. While `rename()` is atomic in memory, the directory block itself must be committed to physical disk using `fsync(dir_fd)`. Otherwise, an immediate power cut can result in an unlinked file or 0-byte directory entry.

---

## 3. Known Weaknesses

1. **Recovery Time is $O(\text{uncompacted log length})$**:
   Although our 4 MB streaming scanner easily replays 1,000,000 keys in ~350 ms, if the database processes 50,000,000 updates without compaction, the uncompacted log could take 10+ seconds to replay on startup.
2. **Double Storage Requirement During Compaction**:
   Compaction uses a copy-on-write strategy to a temporary file (`db.compact.tmp`). Under extreme disk pressure near `max_bytes`, compaction requires temporary free disk space equal to the size of active living keys.
3. **In-Memory Index Sizing**:
   The in-memory hash map stores offsets and lengths for all active keys in RAM. While 1,000,000 keys take only ~28 MB, a database with 100,000,000 keys would require ~3 GB of system RAM for the index.

---

## 4. What We Tried That Failed

1. **Single-Phase Batch Append**:
   * *Initial Attempt*: We appended both batch records and the commit marker in a single contiguous `write()` followed by a single `fsync()`.
   * *Failure Found by Torture Tool*: Under rapid random kills, the child process died while the OS had only flushed the tail sector containing the commit marker. Upon recovery, the parser found a valid commit marker pointing to truncated payload records, violating Invariant Rule 3 ("never partially present").
   * *Fix*: Implemented two-phase sync: fsync the payload records first, then append and fsync the commit marker.
2. **Per-Record `read()` System Calls During Recovery**:
   * *Initial Attempt*: Recovery parsed records using standard per-record reads (`file.Read(headerBuf)`).
   * *Failure*: With 1,000,000 keys, recovery took 3.8 seconds due to 1,000,000 syscall context switches, violating the 2.0-second recovery rule.
   * *Fix*: Implemented a 4 MB buffered streaming reader and zero-allocation key parsing, reducing recovery time to 350 ms.

---

## 5. If We Had 24 More Hours

1. **Persistent Checkpointed Index**:
   Write a snapshot of the in-memory hash index to the tail of the log during clean shutdowns, allowing `open()` to jump straight to the checkpoint and scan only delta records written after it, dropping recovery time to < 10 ms.
2. **Incremental / Log-Structured Segment Compaction**:
   Divide the append-only log into fixed-size 64 MB segments so that compaction can run incrementally on individual segments without requiring full duplicate disk capacity.
3. **Hardware Fallback CRC32 Table**:
   Add an optimized 8-way slicing-by-8 software CRC table fallback for environments where CPU SSE4.2 instructions are unavailable.
