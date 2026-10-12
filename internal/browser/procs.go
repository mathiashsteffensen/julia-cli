package browser

import "syscall"

// proc is a process, identified by pid and start time together, since a
// pid is reused once its process is gone.
type proc struct {
	pid, ppid, pgid int
	start           int64
}

// processTree lists root's live descendants and the members of its process
// group (root included), so Close can kill helpers that outlive the browser
// process: once it exits they are orphans, found only by group.
func processTree(root int) []proc {
	all := processes()
	children := map[int][]proc{}
	var tree []proc
	seen := map[int]bool{}
	for _, p := range all {
		children[p.ppid] = append(children[p.ppid], p)
		if p.pgid == root && p.pid > 1 {
			tree = append(tree, p)
			seen[p.pid] = true
		}
	}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if !seen[c.pid] && c.pid > 1 {
				seen[c.pid] = true
				tree = append(tree, c)
				queue = append(queue, c.pid)
			}
		}
	}
	return tree
}

// kill sends SIGKILL to each process that is still the one listed: same
// pid, same start time.
func kill(procs []proc) {
	live := map[int]int64{}
	for _, p := range processes() {
		live[p.pid] = p.start
	}
	for _, p := range procs {
		if start, ok := live[p.pid]; ok && start == p.start {
			syscall.Kill(p.pid, syscall.SIGKILL)
		}
	}
}
