package browser

import "golang.org/x/sys/unix"

// processes lists the live processes with their parents, groups and start
// times.
func processes() []proc {
	kinfo, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	procs := make([]proc, 0, len(kinfo))
	for i := range kinfo {
		p := &kinfo[i]
		if p.Proc.P_stat == 5 { // SZOMB: exited, waiting to be reaped
			continue
		}
		start := p.Proc.P_starttime
		procs = append(procs, proc{
			pid:   int(p.Proc.P_pid),
			ppid:  int(p.Eproc.Ppid),
			pgid:  int(p.Eproc.Pgid),
			start: int64(start.Sec)*1e6 + int64(start.Usec),
		})
	}
	return procs
}
