package sessionports

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// parsePS parses the output of `ps -axo pid=,ppid=,comm=` into a process
// table. Each non-empty line carries two whitespace-separated integers followed
// by the executable name, which runs to the end of the line and may itself
// contain spaces (the "=" in the format suppresses headers). Lines that do not
// start with two integers are skipped rather than fatal: the process table is
// racy and a single garbled row must not discard the whole snapshot. A truly
// unusable snapshot is the caller's concern (a global process-source failure),
// signalled separately by the command failing, not by this parser.
func parsePS(output []byte) (map[int]Process, error) {
	procs := make(map[int]Process)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	// Process tables can be large; allow generous line buffering.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		pidField, rest := cutField(scanner.Text())
		ppidField, name := cutField(rest)
		pid, err := strconv.Atoi(pidField)
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(ppidField)
		if err != nil {
			continue
		}
		procs[pid] = Process{PPID: ppid, Name: name}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return procs, nil
}

// cutField splits off the first whitespace-delimited field of s and returns it
// with the trimmed remainder.
func cutField(s string) (field, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}
