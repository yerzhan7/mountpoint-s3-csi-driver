package mounter

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/errorfile"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestExitReasons_ExpireAfterTTL(t *testing.T) {
	now := time.Now()
	r := exitReasons{now: func() time.Time { return now }}
	r.record("pv-a", "killed", now)

	now = now.Add(exitReasonTTL)
	_, ok := r.get("pv-a")
	assert.Equals(t, true, ok)

	now = now.Add(time.Second)
	_, ok = r.get("pv-a")
	assert.Equals(t, false, ok)
	assert.Equals(t, 0, len(r.byVolume))
}

// The TTL runs from when the mounter wrote the reason, so reading an old file does not refresh it.
func TestExitReasons_TTLRunsFromWriteTime(t *testing.T) {
	now := time.Now()
	r := exitReasons{now: func() time.Time { return now }}

	r.record("pv-stale", "old", now.Add(-exitReasonTTL-time.Minute))
	_, ok := r.get("pv-stale")
	assert.Equals(t, false, ok)

	written := now.Add(-50 * time.Minute)
	r.record("pv-a", "recent", written)
	got, ok := r.get("pv-a")
	assert.Equals(t, true, ok)
	assert.Equals(t, written, got.At)
	now = now.Add(11 * time.Minute)
	_, ok = r.get("pv-a")
	assert.Equals(t, false, ok)
}

func TestExitReasons_BoundedByCount_DropsOldest(t *testing.T) {
	now := time.Now()
	r := exitReasons{now: func() time.Time { return now }}
	for i := 0; i < maxExitReasons+10; i++ {
		now = now.Add(time.Millisecond)
		r.record(fmt.Sprintf("pv-%d", i), "reason", now)
	}
	assert.Equals(t, maxExitReasons, len(r.byVolume))
	_, ok := r.get("pv-0")
	assert.Equals(t, false, ok)
	_, ok = r.get(fmt.Sprintf("pv-%d", maxExitReasons+9))
	assert.Equals(t, true, ok)

	// Re-recording a kept PV replaces it rather than evicting another.
	r.record("pv-20", "again", now)
	assert.Equals(t, maxExitReasons, len(r.byVolume))
	got, _ := r.get("pv-20")
	assert.Equals(t, "again", got.Reason)
}

// With worst-case Kubernetes names and a full-length reason, the FailedMount event the kubelet builds
// around mountFailedError still carries the whole reason and the hint, within 2 KiB.
func TestMountFailedError_WorstCaseNamesStayReadable(t *testing.T) {
	saved := mounterNamespace
	mounterNamespace = strings.Repeat("n", 63)
	t.Cleanup(func() { mounterNamespace = saved })

	pv := strings.Repeat("p", 253)
	pod := strings.Repeat("m", 253)
	bucket := strings.Repeat("b", 63)
	commDir := "/var/lib/kubelet/pods/" + strings.Repeat("u", 36) + "/volumes/kubernetes.io~empty-dir/comm"
	dm := &DaemonsetMounter{nodeID: "node"}
	dm.mounter.Store(&mounterPod{name: pod, commDir: commDir})

	reason := errorfile.Format("Mountpoint exited with code 1: Error: " + strings.Repeat("cause ", 200) + "ROOT")
	assert.Equals(t, true, len(reason) > errorfile.MaxBytes-5 && len(reason) <= errorfile.MaxBytes)

	target := "/var/lib/kubelet/pods/" + strings.Repeat("w", 36) + "/volumes/kubernetes.io~csi/" + pv + "/mount"
	// pkg/driver/node/node.go wraps Mount's error; the kubelet adds its own prefix.
	nodeErr := fmt.Sprintf("Could not mount %q at %q: %v", bucket, target, dm.mountFailedError(commDir, pv, reason))
	event := fmt.Sprintf("MountVolume.SetUp failed for volume %q : rpc error: code = Internal desc = %s", pv, nodeErr)

	if len(event) > 2048 {
		t.Fatalf("event is %d bytes, want at most 2048", len(event))
	}
	assert.Contains(t, event, reason)
	assert.Contains(t, event, "`kubectl logs -n "+mounterNamespace+" "+pod+" -c mounter | grep -F '["+pv+"]'`")
	t.Logf("worst-case event: %d bytes", len(event))
}

func TestMountFailedError_EmptyReason(t *testing.T) {
	dm := &DaemonsetMounter{nodeID: "node"}
	msg := dm.mountFailedError("/comm", "pv", "").Error()
	assert.Contains(t, msg, "Mountpoint failed: the mounter reported no reason. ")
}
