package codex

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// cacheEntry holds cached TurnRunning result.
type cacheEntry struct {
	size    int64
	modtime time.Time
	running bool
	known   bool
}

var (
	cacheMu sync.Mutex
	cache   = make(map[string]cacheEntry)
	// testChunkReads is incremented for each chunk read in tests; zero means off.
	testChunkReads int
)

// TurnRunning reads the rollout JSONL file backwards, finding the last line with task_started, task_complete, or turn_aborted.
func TurnRunning(path string) (running, known bool) {
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return false, false
	}

	size := fi.Size()
	modtime := fi.ModTime()

	// Check cache.
	cacheMu.Lock()
	if entry, ok := cache[path]; ok && entry.size == size && entry.modtime == modtime {
		cacheMu.Unlock()
		return entry.running, entry.known
	}
	cacheMu.Unlock()

	if size == 0 {
		// Store result in cache.
		cacheMu.Lock()
		cache[path] = cacheEntry{size: size, modtime: modtime, running: false, known: true}
		cacheMu.Unlock()
		return false, true
	}

	const chunkSize = 256 * 1024
	pos := size
	var partial []byte // Partial line carried from previous chunk.

	// Read backwards in 256 KiB chunks.
	for pos > 0 {
		readSize := int64(chunkSize)
		if readSize > pos {
			readSize = pos
		}
		pos -= readSize

		chunk := make([]byte, readSize)
		if _, err := f.ReadAt(chunk, pos); err != nil {
			return false, false
		}
		if testChunkReads > 0 {
			testChunkReads++
		}

		// Build content: this chunk + partial from previous iteration.
		content := append(chunk, partial...)
		contentStr := string(content)

		// Find the last marker in this chunk+partial, scanning backwards.
		lines := strings.Split(contentStr, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := lines[i]
			if marker := isEventMarker(line); marker != "" {
				result := marker == "task_started"
				// Cache and return.
				cacheMu.Lock()
				cache[path] = cacheEntry{size: size, modtime: modtime, running: result, known: true}
				cacheMu.Unlock()
				return result, true
			}
		}

		// Carry forward the first line from this chunk (it might be partial).
		if len(lines) > 0 {
			partial = []byte(lines[0])
		}
	}

	// No marker found, but file was readable.
	cacheMu.Lock()
	cache[path] = cacheEntry{size: size, modtime: modtime, running: false, known: true}
	cacheMu.Unlock()
	return false, true
}

// isEventMarker checks if a line is a valid event_msg with a target type, and returns the type name.
func isEventMarker(line string) string {
	// Cheap prefilter: skip lines that don't contain the marker strings.
	if !strings.Contains(line, "task_started") && !strings.Contains(line, "task_complete") && !strings.Contains(line, "turn_aborted") {
		return ""
	}

	// Parse JSON.
	var ev struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}

	// Check that it's an event_msg with a matching payload type.
	if ev.Type != "event_msg" {
		return ""
	}

	switch ev.Payload.Type {
	case "task_started":
		return "task_started"
	case "task_complete":
		return "task_complete"
	case "turn_aborted":
		return "turn_aborted"
	}
	return ""
}
