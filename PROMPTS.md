# AI Prompt Log (PROMPTS.md)

This log records every prompt sent to AI tools during the development of the database engine and torture suite, in unedited chronological order, adhering to Deliverable 4 of the "Kill It and Prove It" hackathon requirements.

---

### Prompt 1
* **Tool Name**: Antigravity (Gemini 3.8 Flash High)
* **Approximate Time**: 2026-11-06T12:27:50+05:30
* **Prompt**:
```text
Design a binary file format for a single-file key-value database using an append-only log with a header, record entries (magic bytes, length, CRC/checksum, operation type, key, value), and a commit marker. Provide a text-based diagram showing the exact byte offsets and field sizes.
Explain the exact sequence of system calls (write, fsync, writing a commit marker, and fsync of the file/directory) required to guarantee that a batch([ops]) operation is strictly all-or-nothing if the process receives a SIGKILL mid-write.and i need to develop using antigravity and i want use it by github give me step by step method to implement this project

"Outline an algorithm to scan the append-only log upon open(path) from beginning to end under 2 seconds for 1,000,000 keys, how to detect and truncate a torn tail (incomplete trailing write), and how to rebuild the in-memory hash index
```

---

### Prompt 2
* **Tool Name**: Antigravity (Gemini 3.8 Flash High)
* **Approximate Time**: 2026-11-06T12:34:55+05:30
* **Prompt**:
```text
yes
```
*(User authorized initializing repository, scaffold, and Go implementation)*
