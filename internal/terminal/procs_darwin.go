package terminal

import (
	"os"

	"golang.org/x/sys/unix"
)

const zombie = 5 // SZOMB: exited and waiting to be reaped

// members lists the live processes on the terminal: those in the program's
// session or process group, or with the terminal as their controlling tty.
// An interactive shell's jobs lead process groups of their own, and nohup
// keeps a process in the session while it ignores the hangup.
func (s *Session) members() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var pids []int
	for i := range procs {
		p := &procs[i]
		pid := int(p.Proc.P_pid)
		if pid <= 1 || pid == self || p.Proc.P_stat == zombie {
			continue
		}
		if int(p.Eproc.Pgid) == s.pid || int64(p.Eproc.Tdev) == s.tty {
			pids = append(pids, pid)
		} else if sid, err := unix.Getsid(pid); err == nil && sid == s.pid {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// foreground names the process group in the terminal's foreground when it is
// not the program's own: the command a shell is running.
func (s *Session) foreground() string {
	leader, err := unix.SysctlKinfoProc("kern.proc.pid", s.pid)
	if err != nil || leader.Proc.P_stat == zombie {
		return ""
	}
	group := int(leader.Eproc.Tpgid)
	if group <= 0 || group == s.pid {
		return ""
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", group)
	if err != nil || p.Proc.P_stat == zombie {
		return ""
	}
	return unix.ByteSliceToString(p.Proc.P_comm[:])
}
