// Package volumecontext provides utilities for accessing volume context passed via CSI RPC.
package volumecontext

const (
	BucketName           = "bucketName"
	AuthenticationSource = "authenticationSource"
	STSRegion            = "stsRegion"

	// Prefix is the S3 key prefix to mount. It's populated by the CSI Controller Service for
	// dynamically provisioned volumes, statically provisioned volumes use the `--prefix`
	// mount option instead.
	Prefix = "prefix"

	Cache                                = "cache"
	CacheTypeEmptyDir                    = "emptyDir"
	CacheTypeEphemeral                   = "ephemeral"
	CacheEmptyDirSizeLimit               = "cacheEmptyDirSizeLimit"
	CacheEmptyDirMedium                  = "cacheEmptyDirMedium"
	CacheEphemeralStorageClassName       = "cacheEphemeralStorageClassName"
	CacheEphemeralStorageResourceRequest = "cacheEphemeralStorageResourceRequest"

	MountpointPodServiceAccountName = "mountpointPodServiceAccountName"

	MountpointContainerResourcesRequestsCpu    = "mountpointContainerResourcesRequestsCpu"
	MountpointContainerResourcesRequestsMemory = "mountpointContainerResourcesRequestsMemory"
	MountpointContainerResourcesLimitsCpu      = "mountpointContainerResourcesLimitsCpu"
	MountpointContainerResourcesLimitsMemory   = "mountpointContainerResourcesLimitsMemory"

	CSIServiceAccountName   = "csi.storage.k8s.io/serviceAccount.name"
	CSIServiceAccountTokens = "csi.storage.k8s.io/serviceAccount.tokens"
	CSIPodNamespace         = "csi.storage.k8s.io/pod.namespace"
	CSIPodUID               = "csi.storage.k8s.io/pod.uid"
)
