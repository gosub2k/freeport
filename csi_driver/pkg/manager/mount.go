package manager

import (
	"os/exec"
	"strings"
	"syscall"

	"freeport/pkg/devicescan"
)

// maxMountFailures caps how many times ensureMounted retries.
const maxMountFailures = 3

func getDf(mountpoint string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * st.Bsize
}

// isSwapPartition reports whether devPath is formatted as Linux swap.
func isSwapPartition(devPath string) bool {
	// Sysfs doesn't report fs type if its not mounted, use command.
	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", devPath).Output()
	if err != nil {
		return false
	}
	return isSwapType(string(out))
}

func isSwapType(blkidOutput string) bool {
	return strings.TrimSpace(blkidOutput) == "swap"
}

// mountedDevice pairs a discovered device with its mountpoint and free space,
// once mounting has succeeded.
type mountedDevice struct {
	devicescan.Device
	mountpoint string
	free       int64
}
