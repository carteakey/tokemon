# Bug Report: High CPU Usage in Tokemon Agent

## 📋 Problem Description
On macOS (specifically Apple M1/M2 Silicon), the `tokemon agent` process consumes excessive CPU cycles (up to **95.4% CPU**), causing system sluggishness, high load averages, and thermal throttling. 

---

## 🔍 Root Cause Analysis

### 1. Inefficient Adapter Discovery (`filepath.WalkDir` & SQLite Probes)
In `internal/adapters/antigravity/antigravity.go`, the `Discover` function runs a directory walk over the Antigravity conversations directories (`~/.gemini/antigravity-cli/conversations/`):
```go
func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
    // ...
    // Walks the folder and opens EVERY database to check for table compatibility
    supported, _ = probe(ctx, path)
    // ...
}
```
For **every single `.db` file** found in these directories, the `supportsGenerationMetadata` function is called:
```go
func supportsGenerationMetadata(ctx context.Context, path string) (bool, error) {
    db, err := adapters.OpenReadOnly(ctx, path)
    if err != nil {
        return false, err
    }
    defer db.Close()
    return adapters.HasTable(ctx, db, "gen_metadata")
}
```
*   **The Issue:** Opening/closing SQLite connections and querying `sqlite_master` for every database on **every single poll cycle** (default 1 minute) creates massive CPU overhead and high disk I/O.
*   While there are currently only 21 database files, as the number of agent conversations grows to hundreds or thousands, this directory walk and SQLite table query locks up the CPU.

### 2. High Disk I/O & Memory Swap Compounding
Because the M1 Mac's memory is fully saturated (8 GB RAM with 24 active Docker containers), the frequent SQLite file opens trigger constant macOS page-ins/page-outs, causing disk thrashing and inflating the UNIX load average to over **`150.00`**.

### 3. Lack of Exponential Backoff on Server Connection Errors
If the `tokemon` server database is locked, malformed, or unreachable, the agent enters an aggressive retry loop. During these retries, it restarts the local discovery process, repeating the SQLite file probes and compounding the CPU load.

---

## 🛠️ Proposed Solutions & Code Fixes

### Fix 1: Optimize Discovery
Modify `internal/adapters/antigravity/antigravity.go` so it does not open a SQLite connection during the discovery phase. Instead:
1.  Assume any `.db` file in the conversations directory is a potential source.
2.  Delay verification of the `gen_metadata` table to the `Parse` phase, or cache the "supported" status based on the path.
3.  Only probe/parse files if the file modification time (`FileInfo.ModTime()` ) or file size has changed since the last cursor position.

Example optimization in `Discover`:
```diff
- supported, _ = probe(ctx, path)
- a.mu.Lock()
- a.discovery[path] = discoveryEntry{signature: signature, supported: supported}
- a.mu.Unlock()
+ // Skip SQLite open during Discover; just assume .db is a valid candidate
+ supported = true 
```

### Fix 2: Implement Exponential Backoff on Retries
Introduce an exponential backoff wrapper in the agent's main loop when the server returns a `500` error or is unreachable, preventing it from constantly re-polling the disk.
