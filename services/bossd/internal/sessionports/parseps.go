package sessionports

import (
	"bufio"
	"bytes"
	"strconv"
)

// parsePS parses the output of `ps -axo pid=,ppid=` into a pid -> ppid map.
// Each non-empty line carries two whitespace-separated integers (the "=" in
// the format suppresses headers). Lines that do not parse to two integers are
// skipped rather than fatal: the process table is racy and a single garbled
// row must not discard the whole snapshot. A truly unusable snapshot is the
// caller's concern (a global process-source failure), signalled separately by
// the command failing, not by this parser.
func parsePS(output []byte) (map[int]int, error) {
	graph := make(map[int]int)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	// Process tables can be large; allow generous line buffering.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := bytes.Fields(scanner.Bytes())
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(string(fields[0]))
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(string(fields[1]))
		if err != nil {
			continue
		}
		graph[pid] = ppid
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return graph, nil
}
