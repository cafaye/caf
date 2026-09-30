//go:build linux

package cli

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// linuxMemTotal reads MemTotal out of /proc/meminfo, which is in kibibytes. The
// first line is MemTotal by convention in every kernel, and a machine whose
// first line is something else reports zero rather than a wrong number.
func linuxMemTotal() uint64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kib << 10
	}
	return 0
}

// sysctlByName exists so doctor.go compiles everywhere; Linux has no sysctl for
// this and the memory probe does not use it.
func sysctlByName(string) (uint64, error) { return 0, nil }

// totalMemoryBytes is the memory question in this platform's spelling. Linux
// names it `MemTotal` and states it in kibibytes, so the conversion belongs here
// rather than in the caller: a caller that has to know the unit is a caller that
// will eventually forget it.
func totalMemoryBytes() (uint64, error) {
	const kibibyte = 1024
	out, err := sysctlByName("MemTotal")
	if err != nil {
		return 0, err
	}
	return out * kibibyte, nil
}
