package reconcile

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processStart says when process pid started, as a string that differs between two processes that
// had the same pid at different times, or that ran in different boots. ok is false where it cannot
// be told (only Linux can: /proc). A test replaces it.
var processStart = linuxProcessStart

// WorkerName is how this process names itself in the operation journal: "host/pid#start". The start
// makes the name this process's alone: pids are reused (a restarted service is often given the
// pid of the one before it), and a claim of a worker named only by its pid would be taken for a live
// worker's for the whole lease. Where the start cannot be told, the name is "host/pid".
func WorkerName() string {
	host, _ := os.Hostname()
	name := host + "/" + strconv.Itoa(os.Getpid())
	if start, ok := processStart(os.Getpid()); ok {
		name += "#" + start
	}
	return name
}

// workerGone reports whether owner, a worker's name (WorkerName's, or "host/pid" as older versions
// wrote it), is a process of this host that no longer exists, or that is another process with the
// same pid by now. Any other owner may be alive.
func workerGone(owner string) bool {
	i := strings.LastIndex(owner, "/")
	if i < 0 {
		return false
	}
	host, _ := os.Hostname()
	id, start, _ := strings.Cut(owner[i+1:], "#")
	pid, err := strconv.Atoi(id)
	if owner[:i] != host || err != nil || pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		// This process, unless the name is of an earlier one that had the same pid (a restart).
		now, ok := processStart(pid)
		return start != "" && ok && now != start
	}
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	if start == "" {
		return false // written without a start: all that can be told is that the pid is in use
	}
	now, ok := processStart(pid)
	return ok && now != start
}

func linuxProcessStart(pid int) (string, bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	start, ok := startFromStat(stat)
	if !ok {
		return "", false
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(boot)) + ":" + start, true
}

// startFromStat reads the start time (the 22nd field, in clock ticks since boot) from the contents of
// /proc/<pid>/stat. The second field, the command's name, is in parentheses and may hold spaces and
// parentheses itself, so the fields are counted after the last ")".
func startFromStat(stat []byte) (string, bool) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return "", false
	}
	fields := strings.Fields(string(stat[i+1:])) // the 3rd field (the state) first
	if len(fields) < 20 {
		return "", false
	}
	return fields[19], true
}
