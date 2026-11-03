package mounter

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/targetpath"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/errorfile"
)

const (
	// maxErrorFileReadBytes bounds how much of an error file is read; the mounter writes at most errorfile.MaxBytes.
	maxErrorFileReadBytes = 4096
	// exitReasonTTL is how long a PV's last exit reason is kept.
	exitReasonTTL = time.Hour
	// maxExitReasons caps how many PVs' reasons are kept; the oldest is dropped first.
	maxExitReasons = 256
)

// ExitReason is why a PV's Mountpoint on this node last failed or exited, as the mounter reported it.
type ExitReason struct {
	Reason string
	// At is when the mounter wrote the reason (its error file's mtime), not when the node read it.
	At time.Time
}

// exitReasons keeps each PV's last [ExitReason], so it outlives the error file it was read from.
// A Mountpoint that dies after its mount succeeded writes an error file nobody polls; the periodic
// cleanup reads it before removing it, and a later NodePublishVolume of the PV can quote it.
// The zero value is ready to use.
type exitReasons struct {
	mu       sync.Mutex
	byVolume map[string]ExitReason
	now      func() time.Time // nil = time.Now; replaced in tests
}

func (r *exitReasons) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// record keeps reason, written at `at`, as volumeID's last; one already past the TTL is dropped.
func (r *exitReasons) record(volumeID, reason string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byVolume == nil {
		r.byVolume = map[string]ExitReason{}
	}
	now := r.clock()
	r.pruneLocked(now)
	if now.Sub(at) > exitReasonTTL {
		return
	}
	if _, ok := r.byVolume[volumeID]; !ok && len(r.byVolume) >= maxExitReasons {
		oldest := ""
		for id, er := range r.byVolume {
			if oldest == "" || er.At.Before(r.byVolume[oldest].At) {
				oldest = id
			}
		}
		delete(r.byVolume, oldest)
	}
	r.byVolume[volumeID] = ExitReason{Reason: reason, At: at}
}

// forget drops volumeID's reason, once a mount of it succeeds and the reason no longer explains anything.
func (r *exitReasons) forget(volumeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byVolume, volumeID)
}

func (r *exitReasons) get(volumeID string) (ExitReason, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.clock())
	er, ok := r.byVolume[volumeID]
	return er, ok
}

func (r *exitReasons) pruneLocked(now time.Time) {
	for id, er := range r.byVolume {
		if now.Sub(er.At) > exitReasonTTL {
			delete(r.byVolume, id)
		}
	}
}

// LastExitReasonForTarget returns why the Mountpoint serving `target` last failed or exited on this
// node, if the mounter reported it within the last hour. `target` is a NodePublishVolume target path
// (<kubelet>/pods/<pod-uid>/volumes/kubernetes.io~csi/<pv-name>/mount). Reasons are kept per PV name,
// which the path carries; the CSI volume_id (the PV's volumeHandle) is a different value and would never
// match, so this takes the target rather than an ID. A NodePublishVolume that finds the target dead can
// quote it.
func (dm *DaemonsetMounter) LastExitReasonForTarget(target string) (ExitReason, bool) {
	parsed, err := targetpath.Parse(target)
	if err != nil {
		return ExitReason{}, false
	}
	return dm.exitReasons.get(parsed.VolumeID)
}

// consumeErrorFile reads, keeps and removes the error file the mounter wrote for volumeID (the PV name).
// It returns the reason, formatted again in case of an older mounter, and whether there was a file. A
// file whose content formats to nothing still counts. A removal error is returned with the reason.
func (dm *DaemonsetMounter) consumeErrorFile(errFilePath, volumeID string) (string, bool, error) {
	f, err := os.Open(errFilePath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	var reason string
	if err == nil {
		content, readErr := io.ReadAll(io.LimitReader(f, maxErrorFileReadBytes))
		at := time.Now()
		if info, statErr := f.Stat(); statErr == nil {
			at = info.ModTime()
		}
		f.Close()
		if readErr != nil {
			klog.Errorf("DaemonsetMounter: failed to read error file %s: %v", errFilePath, readErr)
		}
		if reason = errorfile.Format(string(content)); reason != "" {
			dm.exitReasons.record(volumeID, reason, at)
		}
	} else {
		klog.Errorf("DaemonsetMounter: failed to open error file %s: %v", errFilePath, err)
	}
	if err := os.Remove(errFilePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return reason, true, err
	}
	return reason, true, nil
}
