package master

import (
	"fmt"
)

const (
	MaxCPULoad  = 85.0 // CPU threshold to block new tasks
	MaxMemLoad  = 90.0 // Memory threshold to block new tasks
	SafeCPULoad = 80.0 // CPU threshold to resume queued tasks
	SafeMemLoad = 85.0 // Memory threshold to resume queued tasks
)

// CheckResourceOverload checks if CPU or memory usage exceeds maximum allowed limits
func CheckResourceOverload(cpu, mem float64) error {
	if cpu > MaxCPULoad {
		return fmt.Errorf("CPU usage %.2f%% exceeds safety threshold (%.1f%%)", cpu, MaxCPULoad)
	}
	if mem > MaxMemLoad {
		return fmt.Errorf("Memory usage %.2f%% exceeds safety threshold (%.1f%%)", mem, MaxMemLoad)
	}
	return nil
}

// IsResourceRecovered returns true if CPU and memory usages are back below safe recovery levels
func IsResourceRecovered(cpu, mem float64) bool {
	return cpu <= SafeCPULoad && mem <= SafeMemLoad
}
