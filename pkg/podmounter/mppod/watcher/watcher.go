// Package watcher provides utilities for watching Mountpoint Pods in the cluster.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// ErrPodNotFound returned when the Mountpoint Pod could not be found in the cluster.
var ErrPodNotFound = errors.New("mppod/watcher: mountpoint pod not found")

// ErrPodNotReady returned when the Mountpoint Pod was found but is not ready.
var ErrPodNotReady = errors.New("mppod/watcher: mountpoint pod not ready")

// ErrCacheDesync returned when the Pod informer cache failed to synchronize within the specified timeout.
var ErrCacheDesync = errors.New("mppod/watcher: failed to sync pod informer cache within the timeout")

// Watcher provides functionality to watch and wait for Mountpoint Pods in the cluster.
// It uses the Kubernetes informer to watch and cache Pod events.
type Watcher struct {
	informer cache.SharedIndexInformer
	lister   listerv1.PodNamespaceLister
}

// New creates a new [Watcher] with the given Kubernetes client, Mountpoint Pod namespace, and resync duration.
func New(client kubernetes.Interface, namespace string, defaultResync time.Duration) *Watcher {
	factory := informers.NewSharedInformerFactoryWithOptions(client, defaultResync, informers.WithNamespace(namespace))
	informer := factory.Core().V1().Pods().Informer()
	lister := factory.Core().V1().Pods().Lister().Pods(namespace)
	return &Watcher{informer, lister}
}

// Start begins watching for Pod events in the cluster.
// It returns [ErrCacheDesync] if the informer cache fails to sync before [stopCh] is cancalled.
// The provided [stopCh] can be used to stop the watching process.
func (w *Watcher) Start(stopCh <-chan struct{}) error {
	go w.informer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, w.informer.HasSynced) {
		return ErrCacheDesync
	}
	return nil
}

// Wait blocks until the specified Mountpoint Pod is found and ready, or until the context is cancelled.
func (w *Watcher) Wait(ctx context.Context, volumeName string, credentialCtx credentialprovider.ProvideContext) (*corev1.Pod, error) {
	// Set a watcher for Pod create & update events
	var podFound atomic.Bool
	podChan := make(chan *corev1.Pod, 1)
	handle, err := w.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pod := obj.(*corev1.Pod)
			if isAssignedMpPod(pod, volumeName, credentialCtx) {
				klog.V(4).Infof("Found MP Pod from Add event informer")
				podFound.Store(true)
				if w.isPodReady(pod) {
					podChan <- pod
				}
			}
		},
		UpdateFunc: func(old, new any) {
			pod := new.(*corev1.Pod)
			if isAssignedMpPod(pod, volumeName, credentialCtx) {
				klog.V(4).Infof("Found MP Pod from Update event informer")
				podFound.Store(true)
				if w.isPodReady(pod) {
					podChan <- pod
				}
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler for %s: %w", volumeName, err)
	}

	// Ensure to remove event handler at the end
	defer w.informer.RemoveEventHandler(handle)

	// Check if the Pod already exists
	labelSelector := labels.Set{
		mppod.LabelVolumeName:           volumeName,
		mppod.LabelAuthenticationSource: credentialprovider.AuthenticationSourceDriver,
		mppod.LabelWorkloadPodFSGroup:   credentialCtx.FSGroup,
	}
	if credentialCtx.AuthenticationSource == credentialprovider.AuthenticationSourcePod {
		labelSelector[mppod.LabelAuthenticationSource] = credentialprovider.AuthenticationSourcePod
		labelSelector[mppod.LabelWorkloadPodNamespace] = credentialCtx.PodNamespace
		labelSelector[mppod.LabelWorkloadPodServiceAccountName] = credentialCtx.ServiceAccountName
	}
	pods, err := w.lister.List(labelSelector.AsSelector())
	if err != nil {
		return nil, fmt.Errorf("failed to list pods %s: %w", volumeName, err)
	}

	var podsOnCurrentNode []*corev1.Pod
	for _, pod := range pods {
		if pod.Spec.NodeName == os.Getenv("CSI_NODE_NAME") {
			podsOnCurrentNode = append(podsOnCurrentNode, pod)
		}
	}

	if len(podsOnCurrentNode) == 1 {
		podFound.Store(true)
		klog.V(4).Infof("Found MP Pod from cache")
		if w.isPodReady(podsOnCurrentNode[0]) {
			// Pod already exists and ready
			return podsOnCurrentNode[0], nil
		}
	} else if len(podsOnCurrentNode) > 1 {
		return nil, fmt.Errorf("found %d MP Pods instead of 1", len(podsOnCurrentNode))
	}

	// Pod does not exists or not ready yet. We set a watcher for create & update events,
	// and will receive the Pod from `podChan` once its ready.
	select {
	case pod := <-podChan:
		// Pod found and ready
		return pod, nil
	case <-ctx.Done():
		// We didn't received the Pod within the timeout

		if podFound.Load() {
			// Pod was found, but was not ready
			return nil, ErrPodNotReady
		}

		return nil, ErrPodNotFound
	}
}

func isAssignedMpPod(mpPod *corev1.Pod, volumeName string, credentialCtx credentialprovider.ProvideContext) bool {
	if mpPod.Spec.NodeName != os.Getenv("CSI_NODE_NAME") {
		return false
	}
	labels := mpPod.Labels
	switch credentialCtx.AuthenticationSource {
	case credentialprovider.AuthenticationSourceUnspecified, credentialprovider.AuthenticationSourceDriver:
		if labels[mppod.LabelVolumeName] == volumeName &&
			labels[mppod.LabelWorkloadPodFSGroup] == credentialCtx.FSGroup &&
			labels[mppod.LabelAuthenticationSource] == credentialprovider.AuthenticationSourceDriver {
			return true
		} else {
			return false
		}
	case credentialprovider.AuthenticationSourcePod:
		if labels[mppod.LabelVolumeName] == volumeName &&
			labels[mppod.LabelWorkloadPodFSGroup] == credentialCtx.FSGroup &&
			labels[mppod.LabelAuthenticationSource] == credentialCtx.AuthenticationSource &&
			labels[mppod.LabelWorkloadPodNamespace] == credentialCtx.PodNamespace &&
			labels[mppod.LabelWorkloadPodServiceAccountName] == credentialCtx.ServiceAccountName {
			return true
		} else {
			return false
		}
	default:
		klog.V(4).Infof("Unknown AuthenticationSource: %s", credentialCtx.AuthenticationSource)
		return false
	}
}

// isPodReady returns whether the given Mountpoint Pod is ready.
func (w *Watcher) isPodReady(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning
}
