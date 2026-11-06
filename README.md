# LogicRush KVDB — Crash-Proof Single-File Key-Value Database

An embedded, crash-safe, single-file key-value storage engine engineered to survive immediate process termination (`kill -9`, `TerminateProcess`, power failure) at any instruction without data loss or torn state.

Designed and built for the **"Kill It and Prove It"** 24-Hour Engineering Hackathon.

---

## 1. How to Build

Building produces two standalone static binaries: the database engine (`kvdb` / `kvdb.exe`) and the torture testing harness (`torture` / `torture.exe`).

### Single Command Build
```bash
# On Linux / macOS:
go build -o kvdb ./src/cmd/kvdb && go build -o torture ./torture

# On Windows (PowerShell / Command Prompt):
go build -o kvdb.exe ./src/cmd/kvdb; go build -o torture.exe ./torture
```
No external packages, CGO, or third-party database libraries are used. Built entirely with Go standard library primitives.

---

## 2. Command-Line Interface (Mandatory 8 Commands)

All commands can be driven directly from the repository root:

```bash
# 1. Open or create database file, run recovery, print key count and elapsed time
./kvdb open <path> [--max-bytes N]

# 2. Store a key-value pair (overwrites if key exists, fsyncs before returning)
./kvdb put <path> <key> <value>

# 3. Retrieve value for key (prints value to stdout, or "NOTFOUND")
./kvdb get <path> <key>

# 4. Remove a key (durable tombstone, idempotent)
./kvdb del <path> <key>

# 5. Apply batch file atomically (file has lines: "put <k> <v>" or "del <k>")
./kvdb batch <path> <batch-file>

# 6. Dump all authoritative keys and values, sorted lexicographically by key
./kvdb dump <path>

# 7. Check file integrity (prints "OK" or reason for failure)
./kvdb check <path>

# 8. Long-running serve daemon for torture test (reads ops from stdin, prints "ACK <id>")
./kvdb serve <path>
```

---

## 3. On-Disk Binary Format

The database authoritative state lives in **exactly one file**. The layout consists of a 32-byte database header followed by an append-only sequence of 32-byte fixed-header records.

### Binary Layout Diagram

```text
========================================================================================================
                                      DATABASE FILE LAYOUT
========================================================================================================
Offset
0x0000 (0)   +-----------------------------------------------------------------------------------------+
             |                                    FILE HEADER (32 Bytes)                               |
             | Magic: "KVLOG_V1" (8B) | Version (2B) | Flags (2B) | Res (4B) | Timestamp (8B) | CRC (4B) |
0x0020 (32)  +-----------------------------------------------------------------------------------------+
             |                          RECORD 1: STANDALONE PUT (Single Op)                           |
             | Header: [MAGIC="REC0", TOTAL_LEN=54, CRC, OP=0x01, FLAGS=0x00, KEY_LEN=6, TX=1, VAL=16] |
             | Payload: Key="user:1" (6B) | Value="alice_2026_active" (16B)                            |
0x0056 (86)  +-----------------------------------------------------------------------------------------+
             |                          RECORD 2: STANDALONE DELETE (Single Op)                        |
             | Header: [MAGIC="REC0", TOTAL_LEN=40, CRC, OP=0x02, FLAGS=0x00, KEY_LEN=8, TX=2, VAL=0]  |
             | Payload: Key="session9" (8B)                                                            |
0x007E (126) +-----------------------------------------------------------------------------------------+
             |                     BATCH TRANSACTION (TX_ID = 3) - 2 OPERATIONS                        |
             |                                                                                         |
             | Record 3a: BATCH PUT                                                                    |
             | Header: [MAGIC="REC0", TOTAL_LEN=52, CRC, OP=0x01, FLAGS=0x01, KEY_LEN=4, TX=3, VAL=16] |
             | Payload: Key="k100" (4B) | Value="val_one_thousand" (16B)                               |
             | - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - |
             | Record 3b: BATCH DELETE                                                                 |
             | Header: [MAGIC="REC0", TOTAL_LEN=36, CRC, OP=0x02, FLAGS=0x01, KEY_LEN=4, TX=3, VAL=0]  |
             | Payload: Key="k101" (4B)                                                                |
             | - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - |
             | Record 3c: COMMIT MARKER FOR TX_ID = 3                                                  |
             | Header: [MAGIC="COMT", TOTAL_LEN=32, CRC, OP=0x03, FLAGS=0x01, K=0, TX=3, V=0, COUNT=2] |
             | Payload: (0 Bytes)                                                                      |
0x010A (266) +-----------------------------------------------------------------------------------------+
```

### Record Header Fields (Little-Endian, 32 bytes)
* `[0x00 - 0x03]` (4B) **Magic**: `0x52454330` (`"REC0"`) for record, `0x434F4D54` (`"COMT"`) for Commit Marker.
* `[0x04 - 0x07]` (4B) **Total Length**: Total bytes of this entry (`32 + KEY_LEN + VAL_LEN`).
* `[0x08 - 0x0B]` (4B) **CRC32-C**: Hardware-accelerated Castagnoli checksum over bytes `[0x0C]` to `[TOTAL_LEN - 1]`.
* `[0x0C]` (1B) **Op Type**: `0x01` = PUT, `0x02` = DELETE, `0x03` = COMMIT_MARKER.
* `[0x0D]` (1B) **Flags**: Bit 0 (`0x01`) = `IS_BATCH_OP` (requires commit marker).
* `[0x0E - 0x0F]` (2B) **Key Length**: Key size (1 to 1024 bytes).
* `[0x10 - 0x17]` (8B) **Transaction ID**: Monotonically increasing batch/op identifier.
* `[0x18 - 0x1B]` (4B) **Value Length**: Value size (0 to 1,048,576 bytes).
* `[0x1C - 0x1F]` (4B) **Batch Count**: Expected number of operations sealed by this commit marker.
* `[0x20 ...]` **Key & Value**: Contiguous byte sequence.

---

## 4. Recovery Algorithm in Plain English

When `./kvdb open <path>` is executed after a normal exit or sudden `SIGKILL`:

1. **Header Validation**: Read the first 32 bytes. If the file is 0 bytes or incomplete, a new 32-byte header is generated and fsynced. If the magic bytes or header CRC32 check fails, recovery rejects the file as corrupted.
2. **Sequential Fast Scan (Sub-2s for 1M Keys)**:
   The log is scanned sequentially using a buffered 4 MB streaming reader. Each record's header is parsed:
   - Check if remaining bytes in file $\ge 32$ (header size).
   - Verify magic bytes (`"REC0"` or `"COMT"`).
   - Verify file has at least `TOTAL_LEN` bytes remaining.
   - Verify hardware-accelerated CRC32-C matches the stored checksum.
3. **Batch Atomicity Guarantee**:
   - Standalone operations (`FLAGS & 0x01 == 0`) update the active index immediately and advance `last_valid_offset`.
   - Batch operations (`FLAGS & 0x01 == 1`) are buffered into a temporary in-memory list (`pending_batch`).
   - When a valid `COMMIT_MARKER` record is parsed:
     * We verify `COMMIT_MARKER.TX_ID == batch.TX_ID` and `COMMIT_MARKER.BATCH_COUNT == len(pending_batch)`.
     * If and only if the commit marker is intact, all buffered batch operations are applied to the active index, and `last_valid_offset` advances to the end of the commit marker.
     * The pending list is cleared.
4. **Torn Tail Detection & Truncation**:
   If the scan encounters unexpected EOF (fewer bytes than header or payload declares), invalid magic, CRC checksum failure, or uncommitted batch records at EOF:
   - The scan terminates immediately.
   - The file is truncated to `last_valid_offset` using `ftruncate()` / `SetEndOfFile`.
   - `fsync()` is executed on the truncated file descriptor.
   - The database file is now clean, uncorrupted, and starts at a valid boundary for future appends.
