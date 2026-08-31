// memory_target.go divides this pod's memory budget between the Mountpoint processes it hosts, so each
// gets a `--memory-target` instead of all of them targeting the whole shared cgroup.
//
// The budget is this container's `limits.memory` when one is set, else its `requests.memory`: a limit
// is the cgroup wall the processes actually share, and with only requests the division sizes them to
// what the scheduler reserved for this pod. Neither being set leaves the budget at 0.
//
// This lives in the mounter, not the CSI Driver Node Pod, because the budget being divided is this
// pod's own: it is projected from the same resources block Kubernetes schedules and enforces, so they
// cannot disagree. It also keeps the number out of the driver's per-volume mount options, which key
// mount sharing and are persisted.

package main

import (
	"fmt"
	"os"
	"strconv"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

const bytesPerMiB = 1024 * 1024

const (
	memoryLimitEnvName = "MOUNTER_MEMORY_LIMIT_BYTES"
	memoryRequestEnvName = "MOUNTER_MEMORY_REQUEST_BYTES"
)

// podMemoryBudgetBytes returns the memory budget to divide between hosted Mountpoints — this
// container's memory limit when it has one, else its memory request — and the resources field it came
// from, so messages can name the knob to turn. 0 means no budget is configured.
func podMemoryBudgetBytes() (budgetBytes int64, budgetField string) {
	if bytes := memoryEnvBytes(memoryLimitEnvName); bytes > 0 {
		return bytes, "limits.memory"
	}
	// An unparseable limit falls through to the request, which Kubernetes validates as never above
	// the limit, so the combined target still fits the cgroup.
	if bytes := memoryEnvBytes(memoryRequestEnvName); bytes > 0 {
		return bytes, "requests.memory"
	}
	return 0, ""
}

func memoryEnvBytes(name string) int64 {
	value := os.Getenv(name)
	if value == "" {
		return 0
	}

	bytes, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		klog.Errorf("Ignoring unparseable %s=%q, expected a plain byte count: %v", name, value, err)
		return 0
	}

	return bytes
}

// resolveMemoryTargetMiB returns each hosted Mountpoint's `--memory-target`, logging the inputs it
// used so `kubectl logs` can answer why a mount was sized the way it was. budgetField names the
// resources field budgetBytes came from ("" when there is no budget), from [podMemoryBudgetBytes].
//
// A non-nil error means the budget cannot afford every volume Mountpoint's minimum, which
// [ProcessManager.Launch] reports per refused mount rather than crashing the pod. Its text is verbose
// because it doubles as that mount's error-file content, all the workload pod's failure event shows.
func resolveMemoryTargetMiB(budgetBytes int64, budgetField string, maxVolumesPerNode int64) (int64, error) {
	targetMiB, undersized := memoryTargetMiB(budgetBytes, maxVolumesPerNode)

	switch {
	case undersized:
		err := fmt.Errorf("the mounter pod's memory budget (resources.%s, %d bytes) divided by "+
			"maxVolumesPerNode=%d gives %d MiB per volume, below Mountpoint's minimum %s of %d MiB, so "+
			"mounts that do not set %s in their PV mountOptions are refused. Raise "+
			"daemonsetMounters[].resources.%s to at least %dMi or lower "+
			"daemonsetMounters[].maxVolumesPerNode",
			budgetField, budgetBytes, maxVolumesPerNode, targetMiB, mountpoint.ArgMemoryTarget,
			mountpoint.MinMemoryTargetMiB, mountpoint.ArgMemoryTarget, budgetField,
			mountpoint.MinMemoryTargetMiB*maxVolumesPerNode)
		klog.Error(err)
		return 0, err
	case targetMiB == 0:
		klog.Warningf("Not setting %s on hosted Mountpoints: memory budget is %d bytes and "+
			"maxVolumesPerNode is %d, so there is nothing to divide. Each Mountpoint that does not set "+
			"%s in its PV mountOptions will instead target 95%% of the memory it detects, so several of "+
			"them in this one cgroup can together exhaust the pod's memory limit — or the node, when it "+
			"has none — and get OOM killed. Set daemonsetMounters[].resources.limits.memory (or "+
			"requests.memory) and a non-zero daemonsetMounters[].maxVolumesPerNode.",
			mountpoint.ArgMemoryTarget, budgetBytes, maxVolumesPerNode, mountpoint.ArgMemoryTarget)
		return 0, nil
	default:
		klog.Infof("Each Mountpoint gets %s=%d (this pod's resources.%s of %d bytes divided by "+
			"maxVolumesPerNode=%d), unless its PV mountOptions set %s themselves.",
			mountpoint.ArgMemoryTarget, targetMiB, budgetField, budgetBytes, maxVolumesPerNode,
			mountpoint.ArgMemoryTarget)
		return targetMiB, nil
	}
}

// memoryTargetMiB divides this pod's memory budget between the maxVolumesPerNode Mountpoints it hosts.
//
// 0 means there was nothing to divide, which leaves each Mountpoint targeting 95% of the memory it
// detects — the whole shared cgroup — unless its PV mountOptions say otherwise.
//
// undersized reports a share below [mountpoint.MinMemoryTargetMiB]; targetMiB is then that share
// itself, for the caller's error message.
func memoryTargetMiB(memoryBudgetBytes int64, maxVolumesPerNode int64) (targetMiB int64, undersized bool) {
	if memoryBudgetBytes <= 0 || maxVolumesPerNode <= 0 {
		return 0, false
	}

	// Floor the share, so the combined target of all volumes never exceeds this pod's budget.
	targetMiB = memoryBudgetBytes / maxVolumesPerNode / bytesPerMiB
	return targetMiB, targetMiB < mountpoint.MinMemoryTargetMiB
}
