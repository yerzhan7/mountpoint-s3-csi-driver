package mounter_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// A Mountpoint that dies after its mount succeeded writes an error file nobody polls; the periodic
// cleanup must keep its reason, retrievable by the target a later NodePublishVolume gets.
func TestLastExitReason_SurvivesPeriodicCleanup(t *testing.T) {
	testCtx := setupDM(t)
	testCtx.mountVolume()
	target := testCtx.targetPath(testCtx.podUID)
	_, ok := testCtx.dm.LastExitReasonForTarget(target)
	assert.Equals(t, false, ok)

	// Mountpoint is OOM-killed: the mounter writes the reason, and the kernel tears the FUSE source down.
	const reason = "Mountpoint was killed by signal SIGKILL"
	errFile := filepath.Join(testCtx.commDir, mounter.GetErrorFileName(testCtx.volumeID))
	assert.NoError(t, os.WriteFile(errFile, []byte(reason), 0600))
	written := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	assert.NoError(t, os.Chtimes(errFile, written, written))
	source := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
	assert.NoError(t, testCtx.mount.Unmount(source))

	testCtx.dm.CleanupOrphans()

	assert.FileNotExists(t, errFile)
	got, ok := testCtx.dm.LastExitReasonForTarget(target)
	assert.Equals(t, true, ok)
	assert.Contains(t, got.Reason, reason)
	// When the mounter wrote it, not when the cleanup read it.
	assert.Equals(t, true, got.At.Equal(written))

	// Keyed by the PV name in the target path, so another pod's target for the same PV finds it too,
	// and a target of another PV does not.
	_, ok = testCtx.dm.LastExitReasonForTarget(testCtx.targetPath("another-pod"))
	assert.Equals(t, true, ok)
	_, ok = testCtx.dm.LastExitReasonForTarget(filepath.Join(testCtx.kubeletPath, "pods", "p", "volumes", "kubernetes.io~csi", "other-pv", "mount"))
	assert.Equals(t, false, ok)
	_, ok = testCtx.dm.LastExitReasonForTarget(testCtx.volumeID)
	assert.Equals(t, false, ok)
}

// The reason a failed start returned to NodePublishVolume is kept too.
func TestLastExitReason_KeptForAFailedStart(t *testing.T) {
	testCtx := setupDM(t)
	testCtx.failMountWithErrorFile("Mountpoint exited with code 1: Error: Failed to create S3 client")

	got, ok := testCtx.dm.LastExitReasonForTarget(testCtx.targetPath(testCtx.podUID))
	assert.Equals(t, true, ok)
	assert.Contains(t, got.Reason, "Error: Failed to create S3 client")
}

// A mount of the PV that succeeds clears its reason, so a later dead-target message cannot quote a
// failure that was already fixed.
func TestLastExitReason_ClearedByASuccessfulMount(t *testing.T) {
	testCtx := setupDM(t)
	testCtx.failMountWithErrorFile("Mountpoint exited with code 1: Error: bucket does not exist")
	_, ok := testCtx.dm.LastExitReasonForTarget(testCtx.targetPath(testCtx.podUID))
	assert.Equals(t, true, ok)

	testCtx.mountVolume()

	_, ok = testCtx.dm.LastExitReasonForTarget(testCtx.targetPath(testCtx.podUID))
	assert.Equals(t, false, ok)
}

// A leftover error file of an earlier Mountpoint no longer explains anything once a new mount succeeds.
func TestLastExitReason_LeftoverFileIsClearedByASuccessfulMount(t *testing.T) {
	testCtx := setupDM(t)
	errFile := filepath.Join(testCtx.commDir, mounter.GetErrorFileName(testCtx.volumeID))
	assert.NoError(t, os.WriteFile(errFile, []byte("Mountpoint exited with code 1: stale"), 0600))

	testCtx.mountVolume()

	assert.FileNotExists(t, errFile)
	_, ok := testCtx.dm.LastExitReasonForTarget(testCtx.targetPath(testCtx.podUID))
	assert.Equals(t, false, ok)
}

// A file found when a mount starts (written after cleanupMount looked, e.g. by a timed-out attempt's
// Mountpoint exiting late) is the PV's newest reason: kept when the new mount then fails without one.
func TestLastExitReason_LateFileKeptWhenTheNextMountTimesOut(t *testing.T) {
	testCtx := setupDM(t)
	errFile := filepath.Join(testCtx.commDir, mounter.GetErrorFileName(testCtx.volumeID))
	assert.NoError(t, os.WriteFile(errFile, []byte("Mountpoint exited with code 1: late reason"), 0600))
	// Mountpoint never serves and writes no error file, so waitForMount ends with the context.
	testCtx.mountSyscall = func(tgt string, opts mpmounter.MountOptions) (int, error) {
		testCtx.mount.Mount("fuse-pending", tgt, "fuse", nil)
		fd, err := syscall.Dup(int(mountertest.OpenDevNull(t).Fd()))
		assert.NoError(t, err)
		return fd, nil
	}
	ctx, cancel := context.WithTimeout(testCtx.ctx, 2*time.Second)
	defer cancel()
	mountRes := make(chan error, 1)
	go func() {
		mountRes <- testCtx.dm.Mount(ctx, testCtx.bucketName, testCtx.targetPath(testCtx.podUID), credentialprovider.ProvideContext{
			WorkloadPodID: testCtx.podUID, VolumeID: testCtx.volumeID,
		}, nil, mountpoint.ParseArgs(nil), "", nil)
	}()
	testCtx.receiveMountOptions()
	assert.Contains(t, (<-mountRes).Error(), "timed out")

	got, ok := testCtx.dm.LastExitReasonForTarget(testCtx.targetPath(testCtx.podUID))
	assert.Equals(t, true, ok)
	assert.Contains(t, got.Reason, "late reason")
}
