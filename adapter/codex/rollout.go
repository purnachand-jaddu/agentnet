package codex

import (
	"os"
	"strings"
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
	if size == 0 {
		return false, true
	}

	const chunkSize = 256 * 1024
	var buffer []byte
	pos := size
	var chunk []byte

	// Read backwards in 256 KiB chunks.
	for pos > 0 {
		readSize := int64(chunkSize)
		if readSize > pos {
			readSize = pos
		}
		pos -= readSize

		chunk = make([]byte, readSize)
		if _, err := f.ReadAt(chunk, pos); err != nil {
			return false, false
		}

		// Prepend this chunk to buffer for line scanning.
		buffer = append(chunk, buffer...)

		// Look for the last line containing our markers.
		if marker := findLastMarker(string(buffer)); marker != "" {
			if marker == "task_started" {
				return true, true
			}
			return false, true
		}
	}

	return false, true
}

// findLastMarker returns the last marker found in the string by searching backwards.
func findLastMarker(content string) string {
	lines := strings.Split(content, "\n")
	// Iterate backwards through lines to find the last marker.
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if strings.Contains(line, "task_started") {
			return "task_started"
		}
		if strings.Contains(line, "task_complete") {
			return "task_complete"
		}
		if strings.Contains(line, "turn_aborted") {
			return "turn_aborted"
		}
	}
	return ""
}
