# Bug Report: High CPU Usage (~86%+) in Tokemon Agent

## 📋 Problem Description
The `tokemon agent` process consumes excessive CPU (up to 86%–96%) on macOS (M1/M2 chips). This issue makes the system run sluggishly and triggers thermal throttling.

---

## 🔍 Root Cause Analysis

### 1. High Disk I/O & CPU overhead during Antigravity Discovery
In `internal/adapters/antigravity/antigravity.go`, the `Discover` function runs a directory walk (`filepath.WalkDir`) over the entire Antigravity conversations directory:
```go
func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	// ...
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		// ...
		supported, err := supportsGenerationMetadata(ctx, path)
		if err == nil && supported {
			sources = append(sources, adapters.Source{Path: path, Identity: adapters.HashIdentity(path)})
		}
		return nil
	})
	// ...
}
```
For **every single database file** found (which can number in the hundreds or thousands for active agent use), it calls `supportsGenerationMetadata`:
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
This opens and closes a SQLite connection to every `.db` file and queries `sqlite_master` on **every single poll interval** (default is 1 minute). This generates significant CPU load and disk I/O.

### 2. Error Loops with Server Failures
When the Tokemon server database is malformed or inaccessible, the server returns a 500 error:
```
tokemon agent: server returned 500 Internal Server Error: {"error":"database disk image is malformed (11)"}
```
The agent repeatedly retries, which compounds the CPU overhead of scanning all directories and databases on each retry cycle.

---

## 🛠️ Suggested Fixes

### 1. Optimize Discovery in the Antigravity Adapter
Avoid opening SQLite connections to files during `Discover`. Instead:
*   Assume any `.db` file in the `conversations` directory is a potential source and check for `gen_metadata` only during the `Parse` phase.
*   Or cache the "supported" status of files (e.g. by path) so they are only opened once instead of on every poll cycle.
*   Only call `Parse` if the file modification time (`FileInfo.ModTime()`) or file size has changed since the last cursor position.

### 2. Add exponential backoff for Server / Network Errors
If the server returns a `500` error or if the connection fails, implement a more aggressive backoff to prevent the agent from thrashing the CPU while repeating local discovery.
