package browser

import (
	"os"
	"strconv"
	"strings"
)

// processes lists the live processes with their parents, groups and start
// times.
func processes() []proc {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var procs []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp … starttime is field 22; comm may hold
		// spaces and parentheses.
		fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
		if len(fields) < 20 || fields[0] == "Z" {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		pgid, _ := strconv.Atoi(fields[2])
		start, _ := strconv.ParseInt(fields[19], 10, 64)
		procs = append(procs, proc{pid: pid, ppid: ppid, pgid: pgid, start: start})
	}
	return procs
}
