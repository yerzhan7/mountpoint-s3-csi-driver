package controller

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

// StorageClass parameters specific to dynamic provisioning.
const (
	// ParamCreationPolicy configures how the CSI Driver obtains the S3 bucket backing the volume.
	ParamCreationPolicy = "creationPolicy"
	// ParamBucketName is the name of the pre-created S3 bucket to provision volumes from.
	ParamBucketName = "bucketName"
	// ParamPrefixPattern is an optional template of the S3 key prefix to provision each volume in,
	// see [resolvePrefixPattern] for the supported placeholders.
	ParamPrefixPattern = "prefixPattern"
)

// Supported values of the [ParamCreationPolicy] parameter.
const (
	// CreationPolicyUseExisting provisions volumes from a pre-created S3 bucket. The CSI Driver
	// doesn't create or delete the bucket, nor the objects within it.
	CreationPolicyUseExisting = "useExisting"
)

// StorageClass parameters injected by the external-provisioner when it's run with `--extra-create-metadata`.
// They carry metadata about the volume being provisioned and are not part of the volume's context.
const (
	paramPVName       = "csi.storage.k8s.io/pv/name"
	paramPVCName      = "csi.storage.k8s.io/pvc/name"
	paramPVCNamespace = "csi.storage.k8s.io/pvc/namespace"

	// csiParamPrefix is the prefix of all StorageClass parameters reserved for the CSI sidecars.
	csiParamPrefix = "csi.storage.k8s.io/"
)

// passthroughParams is the list of PersistentVolume `volumeAttributes` that can also be configured
// as StorageClass parameters. They're passed to the provisioned volume's context as-is.
//
// In addition to these, any `mountpointEnv.`-prefixed parameter is passed through as well.
var passthroughParams = []string{
	volumecontext.AuthenticationSource,
	volumecontext.STSRegion,
	volumecontext.Cache,
	volumecontext.CacheEmptyDirSizeLimit,
	volumecontext.CacheEmptyDirMedium,
	volumecontext.CacheEphemeralStorageClassName,
	volumecontext.CacheEphemeralStorageResourceRequest,
	volumecontext.MountpointPodServiceAccountName,
	volumecontext.MountpointContainerResourcesRequestsCpu,
	volumecontext.MountpointContainerResourcesRequestsMemory,
	volumecontext.MountpointContainerResourcesLimitsCpu,
	volumecontext.MountpointContainerResourcesLimitsMemory,
}

// storageClassParams is a parsed and validated set of StorageClass parameters.
type storageClassParams struct {
	// bucketName is the name of the pre-created S3 bucket backing the volume.
	bucketName string
	// prefix is the resolved S3 key prefix of the volume, empty if the StorageClass doesn't configure one.
	prefix string
	// volumeAttributes are the PersistentVolume `volumeAttributes` passed through from the StorageClass.
	volumeAttributes map[string]string
}

// parseStorageClassParams parses and validates the StorageClass `params` of a `CreateVolume` request
// for the volume named `volumeName` with the StorageClass `mountOptions` given as `mountFlags`.
func parseStorageClassParams(volumeName string, params map[string]string, mountFlags []string) (*storageClassParams, error) {
	parsed := &storageClassParams{volumeAttributes: map[string]string{}}
	prefixPattern := ""
	// The external-provisioner names the PersistentVolume it creates after the requested volume,
	// only the PersistentVolumeClaim's identity needs to be passed via `--extra-create-metadata`.
	metadata := volumeMetadata{pvName: volumeName}

	for key, value := range params {
		switch {
		case key == ParamCreationPolicy:
			if value != CreationPolicyUseExisting {
				return nil, fmt.Errorf("unsupported %s %q, only %q is supported", ParamCreationPolicy, value, CreationPolicyUseExisting)
			}
		case key == ParamBucketName:
			parsed.bucketName = value
		case key == ParamPrefixPattern:
			prefixPattern = value
		case key == paramPVName:
			metadata.pvName = value
		case key == paramPVCName:
			metadata.pvcName = value
		case key == paramPVCNamespace:
			metadata.pvcNamespace = value
		case strings.HasPrefix(key, csiParamPrefix):
			// Other parameters reserved for the CSI sidecars, not part of the volume's context.
		case slices.Contains(passthroughParams, key), strings.HasPrefix(key, envprovider.MountpointEnvPrefix):
			parsed.volumeAttributes[key] = value
		default:
			return nil, fmt.Errorf("unknown parameter %q", key)
		}
	}

	if parsed.bucketName == "" {
		return nil, fmt.Errorf("%s must be provided and non-empty", ParamBucketName)
	}

	if prefixPattern != "" {
		// Both configure the S3 key prefix to mount, and Mountpoint only accepts a single `--prefix`.
		if args := mountpoint.ParseArgs(mountFlags); args.Has(mountpoint.ArgPrefix) {
			return nil, fmt.Errorf("%s parameter and %s mount option are mutually exclusive", ParamPrefixPattern, mountpoint.ArgPrefix)
		}

		prefix, err := resolvePrefixPattern(prefixPattern, metadata)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", ParamPrefixPattern, prefixPattern, err)
		}
		parsed.prefix = prefix
	}

	// Fail here rather than at mount-time if the user configured a disallowed environment variable.
	if _, err := envprovider.ParseUserEnvFromVolumeContext(parsed.volumeAttributes); err != nil {
		return nil, err
	}

	return parsed, nil
}

// volumeContext returns the volume context to pass to the CSI Node Service while mounting the volume.
// It's persisted as the `volumeAttributes` of the provisioned PersistentVolume.
func (p *storageClassParams) volumeContext() map[string]string {
	volumeContext := maps.Clone(p.volumeAttributes)
	volumeContext[volumecontext.BucketName] = p.bucketName
	if p.prefix != "" {
		volumeContext[volumecontext.Prefix] = p.prefix
	}
	return volumeContext
}
