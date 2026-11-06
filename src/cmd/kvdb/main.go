package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"logicrush/src/engine"
	"logicrush/src/format"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "open":
		handleOpen(os.Args[2:])
	case "put":
		handlePut(os.Args[2:])
	case "get":
		handleGet(os.Args[2:])
	case "del":
		handleDel(os.Args[2:])
	case "batch":
		handleBatch(os.Args[2:])
	case "dump":
		handleDump(os.Args[2:])
	case "check":
		handleCheck(os.Args[2:])
	case "serve":
		handleServe(os.Args[2:])
	case "compact":
		handleCompact(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: kvdb <command> [args...]")
	fmt.Println("Commands:")
	fmt.Println("  open <path> [--max-bytes N]  Open or create DB, print key count and recovery time")
	fmt.Println("  put <path> <key> <value>     Store key-value pair")
	fmt.Println("  get <path> <key>             Retrieve key value or NOTFOUND")
	fmt.Println("  del <path> <key>             Delete key")
	fmt.Println("  batch <path> <file>          Apply batch operations from file")
	fmt.Println("  dump <path>                  Print all keys and values, sorted by key")
	fmt.Println("  check <path>                 Verify file integrity, print OK or reason")
	fmt.Println("  serve <path>                 Long-running server reading ops from stdin")
}

func handleOpen(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb open <path> [--max-bytes N]")
		os.Exit(1)
	}
	path := args[0]
	var maxBytes int64 = 0

	for i := 1; i < len(args); i++ {
		if args[i] == "--max-bytes" && i+1 < len(args) {
			val, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Invalid max-bytes: %v\n", err)
				os.Exit(1)
			}
			maxBytes = val
			i++
		}
	}

	start := time.Now()
	db, err := engine.Open(path, maxBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	elapsed := time.Since(start)
	fmt.Printf("Keys: %d, Recovery time: %v\n", db.KeyCount(), elapsed)
}

func handlePut(args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb put <path> <key> <value>")
		os.Exit(1)
	}
	path, key, val := args[0], args[1], args[2]

	db, err := engine.Open(path, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Put([]byte(key), []byte(val)); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func handleGet(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb get <path> <key>")
		os.Exit(1)
	}
	path, key := args[0], args[1]

	db, err := engine.Open(path, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	val, err := db.Get([]byte(key))
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			fmt.Println("NOTFOUND")
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(val))
}

func handleDel(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb del <path> <key>")
		os.Exit(1)
	}
	path, key := args[0], args[1]

	db, err := engine.Open(path, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Delete([]byte(key)); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func handleBatch(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb batch <path> <file>")
		os.Exit(1)
	}
	path, batchFilePath := args[0], args[1]

	f, err := os.Open(batchFilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading batch file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	var ops []engine.BatchOp
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) >= 3 && parts[0] == "put" {
			ops = append(ops, engine.BatchOp{
				Type:  format.OpPut,
				Key:   []byte(parts[1]),
				Value: []byte(parts[2]),
			})
		} else if len(parts) >= 2 && parts[0] == "del" {
			ops = append(ops, engine.BatchOp{
				Type: format.OpDelete,
				Key:  []byte(parts[1]),
			})
		}
	}

	db, err := engine.Open(path, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Batch(ops); err != nil {
		fmt.Fprintf(os.Stderr, "Batch failed: %v\n", err)
		os.Exit(1)
	}
}

func handleDump(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb dump <path>")
		os.Exit(1)
	}
	path := args[0]

	db, err := engine.Open(path, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	pairs, err := db.Dump()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Dump failed: %v\n", err)
		os.Exit(1)
	}

	for _, p := range pairs {
		fmt.Printf("%s %s\n", p.Key, string(p.Value))
	}
}

func handleCheck(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb check <path>")
		os.Exit(1)
	}
	path := args[0]

	db, err := engine.Open(path, 0)
	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Check(); err != nil {
		fmt.Printf("FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func handleCompact(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb compact <path>")
		os.Exit(1)
	}
	path := args[0]

	db, err := engine.Open(path, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Compact(); err != nil {
		fmt.Fprintf(os.Stderr, "Compaction failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func handleServe(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: kvdb serve <path> [--max-bytes N]")
		os.Exit(1)
	}
	path := args[0]
	var maxBytes int64 = 0
	for i := 1; i < len(args); i++ {
		if args[i] == "--max-bytes" && i+1 < len(args) {
			val, _ := strconv.ParseInt(args[i+1], 10, 64)
			maxBytes = val
			i++
		}
	}

	db, err := engine.Open(path, maxBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Serve error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	scanner := bufio.NewScanner(os.Stdin)
	// Buffer up to 2MB lines for large values
	buf := make([]byte, 2*1024*1024)
	scanner.Buffer(buf, 2*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Split(line, " ")
		if len(parts) < 2 {
			continue
		}

		// Support formats:
		// 1) <op-id> put <key> <value>
		// 2) put <op-id> <key> <value>
		// 3) <op-id> del <key>
		// 4) del <op-id> <key>
		// 5) <op-id> batch <count> ...
		var opID, opCmd string
		var rest []string

		if parts[0] == "put" || parts[0] == "del" || parts[0] == "batch" || parts[0] == "compact" {
			opCmd = parts[0]
			opID = parts[1]
			rest = parts[2:]
		} else {
			opID = parts[0]
			opCmd = parts[1]
			rest = parts[2:]
		}

		switch opCmd {
		case "put":
			if len(rest) >= 2 {
				key := rest[0]
				val := strings.Join(rest[1:], " ")
				if err := db.Put([]byte(key), []byte(val)); err == nil {
					fmt.Printf("ACK %s\n", opID)
					os.Stdout.Sync()
				} else {
					fmt.Fprintf(os.Stderr, "ERR %s %v\n", opID, err)
				}
			}
		case "del":
			if len(rest) >= 1 {
				key := rest[0]
				if err := db.Delete([]byte(key)); err == nil {
					fmt.Printf("ACK %s\n", opID)
					os.Stdout.Sync()
				} else {
					fmt.Fprintf(os.Stderr, "ERR %s %v\n", opID, err)
				}
			}
		case "batch":
			if len(rest) >= 1 {
				count, _ := strconv.Atoi(rest[0])
				var bOps []engine.BatchOp
				for i := 0; i < count && scanner.Scan(); i++ {
					subLine := strings.TrimSpace(scanner.Text())
					subParts := strings.SplitN(subLine, " ", 3)
					if len(subParts) >= 3 && subParts[0] == "put" {
						bOps = append(bOps, engine.BatchOp{
							Type:  format.OpPut,
							Key:   []byte(subParts[1]),
							Value: []byte(subParts[2]),
						})
					} else if len(subParts) >= 2 && subParts[0] == "del" {
						bOps = append(bOps, engine.BatchOp{
							Type: format.OpDelete,
							Key:  []byte(subParts[1]),
						})
					}
				}
				if len(bOps) == count {
					if err := db.Batch(bOps); err == nil {
						fmt.Printf("ACK %s\n", opID)
						os.Stdout.Sync()
					} else {
						fmt.Fprintf(os.Stderr, "ERR %s %v\n", opID, err)
					}
				} else {
					fmt.Fprintf(os.Stderr, "ERR %s incomplete batch received (%d of %d)\n", opID, len(bOps), count)
				}
			}
		case "compact":
			if err := db.Compact(); err == nil {
				fmt.Printf("ACK %s\n", opID)
				os.Stdout.Sync()
			} else {
				fmt.Fprintf(os.Stderr, "ERR %s %v\n", opID, err)
			}
		}
	}
}
