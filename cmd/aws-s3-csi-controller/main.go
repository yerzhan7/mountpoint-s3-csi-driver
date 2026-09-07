// `aws-s3-csi-controller` is the entrypoint binary for the CSI Driver's controller component.
// It is responsible for acting on cluster events and spawning Mountpoint Pods when necessary.
// It is also responsible for managing Mountpoint Pods, for example it ensures that completed Mountpoint Pods gets deleted.
//
// It optionally implements CSI's controller service to dynamically provision volumes from
// pre-created Amazon S3 buckets, see `--enable-dynamic-provisioning`.
//
// See /docs/ARCHITECTURE.md for more details.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/awslabs/mountpoint-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/controller"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/version"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
)

var mountpointNamespace = flag.String("mountpoint-namespace", os.Getenv("MOUNTPOINT_NAMESPACE"), "Namespace to spawn Mountpoint Pods in.")
var mountpointVersion = flag.String("mountpoint-version", os.Getenv("MOUNTPOINT_VERSION"), "Version of Mountpoint within the given Mountpoint image.")
var mountpointPriorityClassName = flag.String("mountpoint-priority-class-name", os.Getenv("MOUNTPOINT_PRIORITY_CLASS_NAME"), "Priority class name of the Mountpoint Pods.")
var mountpointPreemptingPriorityClassName = flag.String("mountpoint-preempting-priority-class-name", os.Getenv("MOUNTPOINT_PREEMPTING_PRIORITY_CLASS_NAME"), "Preempting priority class name of the Mountpoint Pods.")
var mountpointHeadroomPriorityClassName = flag.String("mountpoint-headroom-priority-class-name", os.Getenv("MOUNTPOINT_HEADROOM_PRIORITY_CLASS_NAME"), "Priority class name of the Headroom Pods.")
var mountpointImage = flag.String("mountpoint-image", os.Getenv("MOUNTPOINT_IMAGE"), "Image of Mountpoint to use in spawned Mountpoint Pods.")
var headroomImage = flag.String("headroom-image", os.Getenv("MOUNTPOINT_HEADROOM_IMAGE"), "Image of a pause container to use in spawned Headroom Pods.")
var mountpointImagePullPolicy = flag.String("mountpoint-image-pull-policy", os.Getenv("MOUNTPOINT_IMAGE_PULL_POLICY"), "Pull policy of Mountpoint images.")
var mountpointContainerCommand = flag.String("mountpoint-container-command", "/bin/aws-s3-csi-mounter", "Entrypoint command of the Mountpoint Pods.")
var mountpointPodLabels = flag.String("mountpoint-pod-labels", os.Getenv("MOUNTPOINT_POD_LABELS"), "Pod labels to apply to Mountpoint Pods (JSON format).")
var mountpointHeadroomPodLabels = flag.String("mountpoint-headroom-pod-labels", os.Getenv("MOUNTPOINT_HEADROOM_POD_LABELS"), "Pod labels to apply to Headroom Pods (JSON format).")

var mounterMode = flag.String("mounter-mode", os.Getenv("MOUNTER_MODE"), `Mounter mode of the CSI Driver, either "pod" (default) or "daemonset". Mountpoint Pods only exist in "pod" mode.`)
var enableDynamicProvisioning = flag.Bool("enable-dynamic-provisioning", os.Getenv("ENABLE_DYNAMIC_PROVISIONING") == "true",
	`Serve CSI's controller service to dynamically provision volumes from pre-created Amazon S3 buckets. Only supported with the "daemonset" mounter mode.`)
var csiEndpoint = flag.String("csi-endpoint", envOrDefault("CSI_ENDPOINT", "unix:///var/lib/csi/sockets/pluginproxy/csi.sock"),
	"Endpoint to serve CSI's controller service on. Only used if dynamic provisioning is enabled.")

// mounterModeDaemonset is the mounter mode where Mountpoint processes are managed by a
// secondary DaemonSet instead of a Mountpoint Pod per volume.
const mounterModeDaemonset = "daemonset"

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(crdv2.AddToScheme(scheme))
}

func main() {
	flag.Parse()

	logf.SetLogger(zap.New())

	log := logf.Log.WithName(csicontroller.Name)
	conf := config.GetConfigOrDie()

	daemonsetMounterMode := *mounterMode == mounterModeDaemonset

	if *enableDynamicProvisioning && !daemonsetMounterMode {
		log.Error(errors.New("unsupported mounter mode"),
			`Dynamic provisioning is only supported with the "daemonset" mounter mode`, "mounterMode", *mounterMode)
		os.Exit(1)
	}
	if daemonsetMounterMode && !*enableDynamicProvisioning {
		log.Error(errors.New("nothing to reconcile"),
			`The controller component has nothing to do in the "daemonset" mounter mode unless dynamic provisioning is enabled`)
		os.Exit(1)
	}

	mgr, err := manager.New(conf, manager.Options{
		Scheme:                        scheme,
		LeaderElection:                true,
		LeaderElectionID:              "aws-s3-csi-controller",
		LeaderElectionResourceLock:    "leases",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		log.Error(err, "Failed to create a new manager")
		os.Exit(1)
	}

	// Mountpoint Pods only exist in the "pod" mounter mode, there's nothing to reconcile otherwise.
	if !daemonsetMounterMode {
		if err := setupMountpointPodReconciler(mgr, conf, log); err != nil {
			log.Error(err, "Failed to setup Mountpoint Pod reconciler")
			os.Exit(1)
		}
	}

	if *enableDynamicProvisioning {
		csiControllerLog := log.WithName("csi-controller-service")
		csiControllerServer := controller.NewS3ControllerServer(csiControllerLog)
		if err := mgr.Add(controller.NewServer(*csiEndpoint, csiControllerServer, csiControllerLog)); err != nil {
			log.Error(err, "Failed to add CSI controller service to manager")
			os.Exit(1)
		}
	}

	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		log.Error(err, "Failed to start manager")
		os.Exit(1)
	}
}

// setupMountpointPodReconciler configures `mgr` to reconcile Mountpoint Pods for the workload Pods
// using the CSI Driver, and to clean up stale MountpointS3PodAttachments.
func setupMountpointPodReconciler(mgr manager.Manager, conf *rest.Config, log logr.Logger) error {
	if err := crdv2.SetupManagerIndices(mgr); err != nil {
		return fmt.Errorf("failed to setup field indexers: %w", err)
	}

	podLabels := util.ParseLabels(*mountpointPodLabels, log)
	headroomPodLabels := util.ParseLabels(*mountpointHeadroomPodLabels, log)

	reconciler := csicontroller.NewReconciler(mgr.GetClient(), mppod.Config{
		Namespace:                   *mountpointNamespace,
		MountpointVersion:           *mountpointVersion,
		PriorityClassName:           *mountpointPriorityClassName,
		PreemptingPriorityClassName: *mountpointPreemptingPriorityClassName,
		HeadroomPriorityClassName:   *mountpointHeadroomPriorityClassName,
		Container: mppod.ContainerConfig{
			Command:         *mountpointContainerCommand,
			Image:           *mountpointImage,
			HeadroomImage:   *headroomImage,
			ImagePullPolicy: corev1.PullPolicy(*mountpointImagePullPolicy),
		},
		CSIDriverVersion:  version.GetVersion().DriverVersion,
		ClusterVariant:    cluster.DetectVariant(conf, log),
		PodLabels:         podLabels,
		HeadroomPodLabels: headroomPodLabels,
	}, log)

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to create controller: %w", err)
	}

	if err := mgr.Add(csicontroller.NewStaleAttachmentCleaner(reconciler)); err != nil {
		return fmt.Errorf("failed to add stale attachment cleaner to manager: %w", err)
	}

	return nil
}

// envOrDefault returns the value of the `key` environment variable, or `fallback` if it's unset or empty.
func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
