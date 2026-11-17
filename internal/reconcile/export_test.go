package reconcile

// SetProcessStart replaces how a process's start is told, for a test; it returns what restores it.
func SetProcessStart(f func(pid int) (string, bool)) (restore func()) {
	old := processStart
	processStart = f
	return func() { processStart = old }
}

// StartFromStat is startFromStat.
var StartFromStat = startFromStat
