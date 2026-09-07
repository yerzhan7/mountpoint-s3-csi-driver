// Package controller implements the CSI Controller Service of the CSI Driver, which is
// used to dynamically provision PersistentVolumes from pre-created Amazon S3 buckets.
//
// `CreateVolume` turns the parameters of a StorageClass into a volume definition for the
// external-provisioner sidecar to create a PersistentVolume from. `DeleteVolume` is a no-op
// as the CSI Driver doesn't own the lifecycle of the bucket, nor the objects within it.
//
// The service is served over a Unix socket by the `aws-s3-csi-controller` binary, which shares
// the socket with its external-provisioner sidecar. See /docs/ARCHITECTURE.md for more details.
package controller

import (
	"context"
	"slices"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const driverName = "s3.csi.aws.com"

const debugLevel = 4

var controllerCaps = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
}

// volumeCaps is the list of access modes supported by the volumes provisioned by the CSI Driver.
// It has to be kept in sync with the CSI Node Service, see `pkg/driver/node`.
var volumeCaps = []csi.VolumeCapability_AccessMode_Mode{
	csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
}

// S3ControllerServer is the implementation of the [csi.ControllerServer] and [csi.IdentityServer] interfaces.
type S3ControllerServer struct {
	csi.UnimplementedControllerServer
	csi.UnimplementedIdentityServer

	log logr.Logger
}

// NewS3ControllerServer returns a new [S3ControllerServer].
func NewS3ControllerServer(log logr.Logger) *S3ControllerServer {
	return &S3ControllerServer{log: log}
}

// CreateVolume provisions a volume backed by the pre-created S3 bucket configured in the
// StorageClass of the request. The CSI Driver doesn't make any call to Amazon S3 here, it only
// translates the StorageClass parameters into a volume definition, and it's therefore idempotent
// for a given volume name and set of parameters.
func (cs *S3ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	// Only log the fields we act on, `csi.CreateVolumeRequest` also carries secrets.
	cs.log.V(debugLevel).Info("CreateVolume: new request",
		"name", req.GetName(), "parameters", req.GetParameters(), "volumeCapabilities", req.GetVolumeCapabilities())

	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume name not provided")
	}

	volumeCapabilities := req.GetVolumeCapabilities()
	if len(volumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities not provided")
	}
	if !isValidVolumeCapabilities(volumeCapabilities) {
		return nil, status.Errorf(codes.InvalidArgument, "Volume capability not supported, supported access modes are %v", volumeCaps)
	}

	// The external-provisioner passes the StorageClass's `mountOptions` as the mount flags of each
	// requested volume capability, and also copies them into the `mountOptions` of the PersistentVolume
	// it creates. We only need them here to validate them against the StorageClass parameters.
	params, err := parseStorageClassParams(name, req.GetParameters(), mountFlags(volumeCapabilities))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid StorageClass parameters: %v", err)
	}

	cs.log.Info("CreateVolume: volume provisioned",
		"volumeID", name, "bucketName", params.bucketName, "prefix", params.prefix)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId: name,
			// Amazon S3 buckets don't have a capacity, just echo the requested capacity back
			// to let the PersistentVolume report the capacity its PersistentVolumeClaim asked for.
			CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
			VolumeContext: params.volumeContext(),
		},
	}, nil
}

// DeleteVolume deletes given volume. Since the CSI Driver only provisions volumes from pre-created
// buckets, it doesn't own the lifecycle of the bucket nor the objects within it, and there is
// nothing to delete here. It's up to the cluster admins to clean up the bucket and the prefixes in it.
func (cs *S3ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	cs.log.Info("DeleteVolume: nothing to delete, the bucket and the objects within it are not managed by the CSI Driver",
		"volumeID", volumeID)

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *S3ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	volumeCapabilities := req.GetVolumeCapabilities()
	if len(volumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities not provided")
	}
	if !isValidVolumeCapabilities(volumeCapabilities) {
		return &csi.ValidateVolumeCapabilitiesResponse{}, nil
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeCapabilities: volumeCapabilities},
	}, nil
}

func (cs *S3ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := make([]*csi.ControllerServiceCapability, 0, len(controllerCaps))
	for _, cap := range controllerCaps {
		caps = append(caps, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: cap},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// isValidVolumeCapabilities returns whether all given `volumeCapabilities` are supported by the CSI Driver.
func isValidVolumeCapabilities(volumeCapabilities []*csi.VolumeCapability) bool {
	for _, capability := range volumeCapabilities {
		if !slices.Contains(volumeCaps, capability.GetAccessMode().GetMode()) {
			return false
		}
	}
	return true
}

// mountFlags returns all mount flags found in given `volumeCapabilities`.
func mountFlags(volumeCapabilities []*csi.VolumeCapability) []string {
	var flags []string
	for _, capability := range volumeCapabilities {
		flags = append(flags, capability.GetMount().GetMountFlags()...)
	}
	return flags
}
