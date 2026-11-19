//go:build linux || darwin

package agent

import "syscall"

// freeSpace - сколько байт доступно непривилегированному процессу в каталоге.
func freeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// diskUsedPct - занятое место файловой системы каталога, % (как в df: от места,
// доступного непривилегированному процессу).
func diskUsedPct(dir string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	used := float64(uint64(st.Blocks)-uint64(st.Bfree)) * float64(st.Bsize)
	avail := float64(uint64(st.Bavail)) * float64(st.Bsize)
	if used+avail <= 0 {
		return 0, nil
	}
	return used / (used + avail) * 100, nil
}
