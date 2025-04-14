package csicontroller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
)

const debugLevel = 4

const mountpointCSIDriverName = "s3.csi.aws.com"

type Expectations struct {
	pending sync.Map
}

func NewExpectations() *Expectations {
	return &Expectations{}
}
func (e *Expectations) SetPending(key string) {
	e.pending.Store(key, struct{}{})
}

func (e *Expectations) IsPending(key string) bool {
	_, ok := e.pending.Load(key)
	return ok
}

func (e *Expectations) Clear(key string) {
	e.pending.Delete(key)
}

// A Reconciler reconciles Mountpoint Pods by watching other workload Pods thats using S3 CSI Driver.
type Reconciler struct {
	mountpointPodConfig       mppod.Config
	mountpointPodCreator      *mppod.Creator
	mountpointPodExpectations *Expectations

	client.Client
}

// NewReconciler returns a new reconciler created from `client` and `podConfig`.
func NewReconciler(client client.Client, podConfig mppod.Config) *Reconciler {
	creator := mppod.NewCreator(podConfig)
	return &Reconciler{Client: client, mountpointPodExpectations: NewExpectations(), mountpointPodConfig: podConfig, mountpointPodCreator: creator}
}

// SetupWithManager configures reconciler to run with given `mgr`.
// It automatically configures reconciler to reconcile Pods in the cluster.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(Name).
		For(&corev1.Pod{}).
		Complete(r)
}

// Reconcile reconciles either a Mountpoint- or a workload-Pod.
//
// For Mountpoint Pods, it deletes completed Pods and logs each status change.
// For workload Pods, it decides if it needs to spawn a Mountpoint Pod to provide a volume for the workload Pod.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("pod", req.NamespacedName)

	pod := &corev1.Pod{}
	err := r.Get(ctx, req.NamespacedName, pod)
	if err != nil {
		// This is not an error situation as sometimes we schedule retries for `req`s,
		// and they might got deleted once we try to re-process them again.
		if apierrors.IsNotFound(err) {
			log.Info("Pod not found - ignoring")
			return reconcile.Result{}, nil
		}
		log.Error(err, "Failed to get Pod")
		return reconcile.Result{}, err
	}

	if r.isMountpointPod(pod) {
		return r.reconcileMountpointPod(ctx, pod)
	}

	return r.reconcileWorkloadPod(ctx, pod)
}

// reconcileMountpointPod reconciles given Mountpoint `pod`, and deletes it if its completed.
func (r *Reconciler) reconcileMountpointPod(ctx context.Context, pod *corev1.Pod) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("mountpointPod", pod.Name)

	switch pod.Status.Phase {
	case corev1.PodPending:
		log.V(debugLevel).Info("Pod pending to be scheduled")
	case corev1.PodRunning:
		log.V(debugLevel).Info("Pod is running")
	case corev1.PodSucceeded:
		err := r.deleteMountpointPod(ctx, pod)
		if err != nil {
			log.Error(err, "Failed to delete succeeded Pod")
			return reconcile.Result{}, err
		}
		log.Info("Pod succeeded and successfully deleted")
	case corev1.PodFailed:
		// TODO: We should probably delete failed Pods after some time to trigger a retry on the whole operation.
		//       Maybe just returning a `reconcile.Result{RequeueAfter: ...}`
		//       and deleting in next cycle would be a good way?
		log.Info("Pod failed", "reason", pod.Status.Reason)
	}

	return reconcile.Result{}, nil
}

// reconcileWorkloadPod reconciles given workload `pod` to spawn a Mountpoint Pod to provide a volume for it if needed.
func (r *Reconciler) reconcileWorkloadPod(ctx context.Context, pod *corev1.Pod) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("pod", types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})

	if pod.Spec.NodeName == "" {
		log.V(debugLevel).Info("Pod is not scheduled to a node yet - ignoring")
		return reconcile.Result{}, nil
	}

	if len(pod.Spec.Volumes) == 0 {
		log.V(debugLevel).Info("Pod has no volumes - ignoring")
		return reconcile.Result{}, nil
	}

	var requeue bool
	var errs []error

	for _, vol := range pod.Spec.Volumes {
		podPVC := vol.PersistentVolumeClaim
		if podPVC == nil {
			continue
		}

		// If PVC has no bound PVs yet, `getBoundPVForPodClaim` will return `errPVCIsNotBoundToAPV`.
		// In this case we'll just return `reconcile.Result{Requeue: true}` here, which will bubble up to the
		// original `Reconcile` call and will cause a retry for this Pod with an exponential backoff.
		pvc, pv, err := r.getBoundPVForPodClaim(ctx, pod, podPVC)
		if err != nil {
			if errors.Is(err, errPVCIsNotBoundToAPV) {
				requeue = true
			} else {
				errs = append(errs, err)
			}
			continue
		}

		csiSpec := extractCSISpecFromPV(pv)
		if csiSpec == nil {
			continue
		}

		log.V(debugLevel).Info("Found bound PV for PVC", "pvc", pvc.Name, "volumeName", pv.Name)

		needsRequeue, err := r.spawnOrDeleteMountpointPodIfNeeded(ctx, pod, pvc, pv, csiSpec)
		requeue = requeue || needsRequeue
		if err != nil {
			errs = append(errs, err)
			continue
		}
	}

	return reconcile.Result{Requeue: requeue}, errors.Join(errs...)
}

// spawnOrDeleteMountpointPodIfNeeded spawns or deletes existing Mountpoint Pod for given `workloadPod` and volume if needed.
//
// If `workloadPod` is `Pending` and without any associated Mountpoint Pod, a new Mountpoint Pod will be created for it to provide volume.
//
// If `workloadPod` is `Pending` and scheduled for termination (i.e., `DeletionTimestamp` is non-nil), and there is an existing Mountpoint Pod for it,
// the Mountpoint Pod will be scheduled for termination as well. This is because if `workloadPod` never transition into its `Running` state,
// the Mountpoint Pod might never got a successful mount operation, and thus it might never get unmount operation to cleanly exit
// and might hang there until it reaches its timeout. We just terminate it in this case to prevent unnecessary waits.
func (r *Reconciler) spawnOrDeleteMountpointPodIfNeeded(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pvc *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
	csiSpec *corev1.CSIPersistentVolumeSource,
) (bool, error) {
	// `workloadPod` is not active, its either terminated (i.e., `phase == Succeeded or phase == Failed`) or
	// its scheduled for termination (i.e., `DeletionTimestamp != nil`)
	if !isPodActive(workloadPod) {
		return false, nil
	}

	volumeAttributes := mppod.ExtractVolumeAttributes(pv)
	authSource := volumeAttributes[volumecontext.AuthenticationSource]
	if authSource == "" { // TODO: This is duplicate logic with credential provider. We can refactor it.
		authSource = "driver"
	}
	fsGroup := ""
	if workloadPod.Spec.SecurityContext.FSGroup != nil {
		fsGroup = strconv.FormatInt(*workloadPod.Spec.SecurityContext.FSGroup, 10)
	}

	log := logf.FromContext(ctx).WithValues(
		"workloadPod", types.NamespacedName{Namespace: workloadPod.Namespace, Name: workloadPod.Name},
		"authSource", authSource,
		"pvc", pvc.Name, "volumeName", pv.Name)

	mpPodList := &corev1.PodList{}
	labelFilters := client.MatchingLabels{
		mppod.LabelVolumeName:           pv.Name,
		mppod.LabelAuthenticationSource: authSource,
		mppod.LabelWorkloadPodFSGroup:   fsGroup,
	}

	if authSource == "pod" {
		labelFilters[mppod.LabelWorkloadPodNamespace] = workloadPod.Namespace
		labelFilters[mppod.LabelWorkloadPodServiceAccountName] = workloadPod.Spec.ServiceAccountName
	}
	fieldFilters := client.MatchingFields{"spec.nodeName": workloadPod.Spec.NodeName}

	expectKey := deriveKeyFromFilters(labelFilters, workloadPod.Spec.NodeName)
	err := r.List(ctx, mpPodList, client.InNamespace(r.mountpointPodConfig.Namespace), fieldFilters, labelFilters)

	if err != nil {
		log.Error(err, "Failed to list Mountpoint Pods")
		return false, err
	}

	if len(mpPodList.Items) > 1 {
		err := fmt.Errorf("found %d Mountpoint Pods on Node %s instead of 1", len(mpPodList.Items), workloadPod.Spec.NodeName)
		log.Error(err, "Unexpected number of Mountpoint Pods found")
		return false, err
	}

	if len(mpPodList.Items) == 1 {
		log.Info("Mountpoint Pod already exists - ignoring")

		if r.mountpointPodExpectations.IsPending(expectKey) {
			log.Info("IS_PENDING=TRUE - REMOVE FROM PENDING - " + expectKey)
			r.mountpointPodExpectations.Clear(expectKey)
		}

		return false, nil
	} else {
		log.Info("Mountpoint Pod does not exist - checking if workload Pod is pending")
		if r.mountpointPodExpectations.IsPending(expectKey) {
			log.Info("IS_PENDING=TRUE - DO NOT SPAWN ANOTHER MPPOD - REQUEUE - " + expectKey)
			return true, nil
		} else {
			log.Info("IS_PENDING=FALSE - SPAWN NEW MPPOD - REQUEUE - " + expectKey)

			if err := r.spawnMountpointPod(ctx, workloadPod, pvc, pv, csiSpec, authSource); err != nil {
				log.Error(err, "Failed to spawn Mountpoint Pod")
				return false, err
			}
			log.Info("SET PENDING - REQUEUE - " + expectKey)
			r.mountpointPodExpectations.SetPending(expectKey)

			return true, nil
		}
	}
}

// spawnMountpointPod spawns a new Mountpoint Pod for given `workloadPod` and volume.
// The Mountpoint Pod will be spawned into the same node as `workloadPod`, which then the mount operation
// will be continued by the CSI Driver Node component in that node.
func (r *Reconciler) spawnMountpointPod(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pvc *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
	_ *corev1.CSIPersistentVolumeSource,
	authSource string,
) error {
	log := logf.FromContext(ctx).WithValues(
		"workloadPod", types.NamespacedName{Namespace: workloadPod.Namespace, Name: workloadPod.Name},
		"authSource", authSource,
		"pvc", pvc.Name, "volumeName", pv.Name)

	log.Info("Spawning Mountpoint Pod")

	mpPod := r.mountpointPodCreator.Create(workloadPod, pv)
	err := r.Create(ctx, mpPod)
	if err != nil {
		log.Error(err, "Failed to create Mountpoint Pod")
		return err
	}

	log.Info("Mountpoint Pod spawned", "mountpointPodUID", mpPod.UID, "mountpointPodName", mpPod.Name)
	return nil
}

func deriveKeyFromFilters(labelFilters client.MatchingLabels, nodeName string) string {
	keys := make([]string, 0, len(labelFilters))
	for k := range labelFilters {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(labelFilters[k])
		sb.WriteString(";")
	}
	return nodeName + sb.String()
}

// deleteMountpointPod deletes given `mountpointPod`.
// It does not return an error if `mountpointPod` does not exists in the control plane.
func (r *Reconciler) deleteMountpointPod(ctx context.Context, mountpointPod *corev1.Pod) error {
	log := logf.FromContext(ctx).WithValues("mountpointPod", mountpointPod.Name)

	err := r.Delete(ctx, mountpointPod)
	if err == nil {
		log.Info("Mountpoint Pod deleted")
		return nil
	}

	if apierrors.IsNotFound(err) {
		log.Info("Mountpoint Pod has been deleted already")
		return nil
	}

	log.Error(err, "Failed to delete Mountpoint Pod")
	return err
}

// errPVCIsNotBoundToAPV is returned when given PVC is not bound to a PV yet.
// This is not a terminal error - as PVCs can be bound to PVs dynamically - and just a transient error
// to be retried later.
var errPVCIsNotBoundToAPV = errors.New("PVC is not bound to a PV yet")

// getBoundPVForPodClaim tries to find bound PV and PVC from given `claim`.
// It `errPVCIsNotBoundToAPV` if PVC is not bound to a PV yet to be eventually retried.
func (r *Reconciler) getBoundPVForPodClaim(
	ctx context.Context,
	pod *corev1.Pod,
	claim *corev1.PersistentVolumeClaimVolumeSource,
) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, error) {
	log := logf.FromContext(ctx).WithValues("pod", types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, "pvc", claim.ClaimName)

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: claim.ClaimName}, pvc)
	if err != nil {
		log.Error(err, "Failed to get PVC for Pod")
		return nil, nil, fmt.Errorf("Failed to get PVC for Pod: %w", err)
	}

	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		log.V(debugLevel).Info("PVC is not bound to a PV yet or has a empty volume name - ignoring",
			"status", pvc.Status.Phase,
			"volumeName", pvc.Spec.VolumeName)
		return nil, nil, errPVCIsNotBoundToAPV
	}

	pv := &corev1.PersistentVolume{}
	err = r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv)
	if err != nil {
		log.Error(err, "Failed to get PV bound to PVC", "volumeName", pvc.Spec.VolumeName)
		return nil, nil, fmt.Errorf("Failed to get PV bound to PVC: %w", err)
	}

	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Name != pvc.Name {
		log.Info("Found the PV but its `ClaimRef` is not bound to the PVC", "volumeName", pvc.Spec.VolumeName)
		return nil, nil, errors.New("The PV has a different `ClaimRef` than the PVC")
	}

	return pvc, pv, nil
}

// isMountpointPod returns whether given `pod` is a Mountpoint Pod.
// It currently checks namespace of `pod`.
func (r *Reconciler) isMountpointPod(pod *corev1.Pod) bool {
	// TODO: Do we need to perform any additional check here?
	return pod.Namespace == r.mountpointPodConfig.Namespace
}

// extractCSISpecFromPV tries to extract `CSIPersistentVolumeSource` from given `pv`.
// It returns nil if the CSI Driver used in the `pv` is not S3 CSI Driver.
func extractCSISpecFromPV(pv *corev1.PersistentVolume) *corev1.CSIPersistentVolumeSource {
	csi := pv.Spec.CSI
	if csi == nil || csi.Driver != mountpointCSIDriverName {
		return nil
	}
	return csi
}

// isPodActive returns whether given Pod is active and not in the process of termination.
// Copied from https://github.com/kubernetes/kubernetes/blob/8770bd58d04555303a3a15b30c245a58723d0f4a/pkg/controller/controller_utils.go#L1009-L1013.
func isPodActive(p *corev1.Pod) bool {
	return corev1.PodSucceeded != p.Status.Phase &&
		corev1.PodFailed != p.Status.Phase &&
		p.DeletionTimestamp == nil
}
