package csicontroller

import (
	"context"
	"fmt"
	"time"

	s3v1alpha1 "github.com/aws-controllers-k8s/s3-controller/apis/v1alpha1"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/version"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Name of this component, this needs to be unique as its used as an identifier in logs and metrics.
const Name = "aws-s3-csi-controller"

var log = logf.Log.WithName(Name)

const (
	// CreateBucketParameter controls whether CSI driver should create Bucket CRD
	CreateBucketParameter = "createBucket"
	// UniqueBucketPerVolumeParameter controls whether CSI driver should create/use one bucket per PVC
	UniqueBucketPerVolumeParameter = "uniqueBucketPerVolume"
	// BucketNamePrefixParameter is used to specify a prefix for generated bucket names
	BucketNamePrefixParameter = "bucketNamePrefix"
	// EnablePVCPathIsolationParameter controls whether to add PVC namespace/name as prefix in mountpoint for isolation
	EnablePVCPathIsolationParameter = "enablePVCPathIsolation"
	// BucketDefinitionParameter contains the complete ACK S3 Bucket CRD definition
	BucketDefinitionParameter = "bucketDefinition"
)

type Controller struct {
	csi.ControllerServer
	Client client.Client
}

func (d *Controller) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	log.Info("CreateVolume: called with args", "request", req)
	// TODO: Add volume lock
	volumeId := req.Name

	// Create volume context
	volContext := map[string]string{}

	// Get parameters from StorageClass
	params := req.GetParameters()

	// Check if we should create a bucket or use an existing one
	createBucket := true
	if value, exists := params[CreateBucketParameter]; exists {
		createBucket = value == "true"
	}

	// Check if we should create a unique bucket per volume
	uniqueBucketPerVolume := true
	if value, exists := params[UniqueBucketPerVolumeParameter]; exists {
		uniqueBucketPerVolume = value == "true"
	}

	// Check if we need PVC path isolation (for shared buckets)
	enablePVCPathIsolation := false
	if value, exists := params[EnablePVCPathIsolationParameter]; exists {
		enablePVCPathIsolation = value == "true"
	}

	// Add path isolation if enabled for shared bucket
	if !uniqueBucketPerVolume && enablePVCPathIsolation {
		// Use PVC namespace and name for better isolation
		pvcName, hasPvcName := params["csi.storage.k8s.io/pvc/name"]
		pvcNamespace, hasPvcNamespace := params["csi.storage.k8s.io/pvc/namespace"]

		if !hasPvcName || !hasPvcNamespace {
			return nil, status.Errorf(codes.InvalidArgument, "PVC name and namespace are required for path isolation but missing from request parameters")
		}

		// Format: namespace/name/
		volContext[volumecontext.VolumePathPrefix] = pvcNamespace + "/" + pvcName + "/"
		log.Info("Using PVC namespace and name for path isolation", "prefix", volContext[volumecontext.VolumePathPrefix])
	}

	var bucketName string
	var bucketNamespace string = "default" // Default namespace

	if createBucket {
		// Get bucket name prefix if specified
		prefix := ""
		if p, exists := params[BucketNamePrefixParameter]; exists && p != "" {
			prefix = p + "-"
		}

		// Parse bucket definition from parameters
		mapper := &ParameterMapper{}
		bucketDefYAML, exists := params[BucketDefinitionParameter]
		if !exists || bucketDefYAML == "" {
			return nil, status.Errorf(codes.InvalidArgument, "Missing required parameter: %s", BucketDefinitionParameter)
		}

		bucketObj, err := mapper.MapToBucketObject(bucketDefYAML)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Failed to parse bucket configuration: %v", err)
		}

		// Validate that bucketNamePrefix is not used together with explicitly set name
		if prefix != "" && bucketObj.GetName() != "" {
			return nil, status.Errorf(codes.InvalidArgument,
				"Cannot specify bucket name (via metadata.name) at the same time as using bucketNamePrefix")
		}

		specNameRaw, hasSpecName, _ := unstructured.NestedString(bucketObj.Object, "spec", "name")
		if prefix != "" && hasSpecName && specNameRaw != "" {
			return nil, status.Errorf(codes.InvalidArgument,
				"Cannot specify bucket name (via spec.name) at the same time as using bucketNamePrefix")
		}

		// Extract namespace from the bucket object if specified
		if bucketObj.GetNamespace() != "" {
			bucketNamespace = bucketObj.GetNamespace()
		}

		// For unique bucket per volume, we generate a name with prefix and volumeId
		if uniqueBucketPerVolume {
			// TODO: What to do there is bucket naming collision and bucket already exists?
			bucketName = prefix + volumeId

			// Set the bucket name in the bucket object
			bucketObj.SetName(bucketName)
			// Also set spec.name via unstructured access
			if err := unstructured.SetNestedField(bucketObj.Object, bucketName, "spec", "name"); err != nil {
				return nil, status.Errorf(codes.Internal, "Failed to set bucket name in spec: %v", err)
			}
		} else {
			// For shared bucket, we must have a bucket name in the definition
			// The name extraction will be handled by mapper.MapToBucketObject

			// Extract the bucket name from the object
			// Will need to extract from the bucketObj
			// Placeholder for now
			bucketName = bucketObj.GetName()

			if bucketName == "" {
				return nil, status.Errorf(codes.InvalidArgument,
					"Bucket name must be specified in bucketDefinition when uniqueBucketPerVolume is false")
			}

			// TODO: Do not fail creation of CRD Bucket if it already exist
			// TODO: OR should we create CRD Bucket when using shared bucket? Should we just ask customer to create it first?
		}

		// Add labels to the bucket object
		// TODO: Add CSI Driver version as label
		labels := map[string]string{
			"s3.csi.aws.com/created-by": "aws-mountpoint-s3-csi-driver",
		}

		// Add PVC and PV information if available
		if pvcName, ok := params["csi.storage.k8s.io/pvc/name"]; ok {
			labels["s3.csi.aws.com/pvc-name"] = pvcName
		}
		if pvcNamespace, ok := params["csi.storage.k8s.io/pvc/namespace"]; ok {
			labels["s3.csi.aws.com/pvc-namespace"] = pvcNamespace
		}
		if pvName, ok := params["csi.storage.k8s.io/pv/name"]; ok {
			labels["s3.csi.aws.com/pv-name"] = pvName
		}

		// Add storage class parameters as labels
		labels["s3.csi.aws.com/unique-bucket-per-volume"] = fmt.Sprintf("%t", uniqueBucketPerVolume)
		labels["s3.csi.aws.com/enable-pvc-path-isolation"] = fmt.Sprintf("%t", enablePVCPathIsolation)

		// Set labels on the bucket object
		existingLabels := bucketObj.GetLabels()
		if existingLabels == nil {
			existingLabels = map[string]string{}
		}
		for k, v := range labels {
			existingLabels[k] = v
		}
		bucketObj.SetLabels(existingLabels)

		// Create the bucket using client
		err = d.Client.Create(ctx, bucketObj)
		if err != nil {
			// TODO: Handle case if CRD/Bucket already exist
			return nil, status.Errorf(codes.Internal, "Failed to create bucket CRD: %v", err)
		}

		// Wait for bucket creation
		if err := d.waitForBucketCreation(ctx, bucketObj.GetName(), bucketNamespace); err != nil {
			return nil, status.Errorf(codes.Internal, "Failed waiting for bucket creation: %v", err)
		}
	} else {
		// When not creating a bucket, we must have a bucket name in the definition
		// Parse bucket definition from parameters to get the name
		mapper := &ParameterMapper{}
		bucketDefYAML, exists := params[BucketDefinitionParameter]
		if !exists || bucketDefYAML == "" {
			return nil, status.Errorf(codes.InvalidArgument, "Missing required parameter: %s", BucketDefinitionParameter)
		}

		bucketObj, err := mapper.MapToBucketObject(bucketDefYAML)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Failed to parse bucket configuration: %v", err)
		}

		// Extract the bucket name from the object
		bucketName = bucketObj.GetName()

		if bucketName == "" {
			return nil, status.Errorf(codes.InvalidArgument,
				"Bucket name must be specified in bucketDefinition when createBucket is false")
		}

		// Extract namespace from the bucket object if specified
		if bucketObj.GetNamespace() != "" {
			bucketNamespace = bucketObj.GetNamespace()
		}

		log.Info("Using existing bucket", "bucket", bucketName, "namespace", bucketNamespace)
	}

	volContext[volumecontext.BucketName] = bucketName

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeId,
			CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
			VolumeContext: volContext,
		},
	}, nil
}

func (d *Controller) waitForBucketCreation(ctx context.Context, bucketName, namespace string) error {
	tick := time.Tick(1 * time.Second)
	timeout := time.After(5 * time.Minute)

	for {
		select {
		case <-tick:
			// Get bucket using unstructured object to handle any CRD version
			bucket := &s3v1alpha1.Bucket{}
			err := d.Client.Get(ctx, types.NamespacedName{
				Namespace: namespace,
				Name:      bucketName,
			}, bucket)
			if err != nil {
				log.Error(err, "Failed to get bucket status")
				continue
			}

			// Check if the bucket is ready
			for _, condition := range bucket.Status.Conditions {
				if condition.Type == "ACK.ResourceSynced" {
					switch condition.Status {
					case "True":
						log.Info("Bucket creation completed successfully", "bucket", bucketName)
						return nil
					case "False":
						if condition.Message != nil {
							return fmt.Errorf("bucket creation failed: %s", *condition.Message)
						}
						return fmt.Errorf("bucket creation failed without error message")
					}
				}
			}

		case <-timeout:
			return fmt.Errorf("timeout waiting for bucket creation")

		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for bucket creation: %v", ctx.Err())
		}
	}
}

func (d *Controller) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	log.Info("DeleteVolume: called with args", "request", req)
	// TODO: Implement bucket deletion based on volume ID? Or do not support this at all, and let customers to perform bucket cleanup.
	return &csi.DeleteVolumeResponse{}, nil
}

func (d *Controller) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Controller) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Controller) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	log.Info("ControllerGetCapabilities: called with args %#v", req)
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	}
	var capsResponse []*csi.ControllerServiceCapability
	for _, cap := range caps {
		c := &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		capsResponse = append(capsResponse, c)
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: capsResponse}, nil
}

func (d *Controller) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	log.Info("GetCapacity: called with args %#v", req)
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Controller) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	log.Info("ListVolumes: called with args %#v", req)
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Controller) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	log.Info("ValidateVolumeCapabilities: called with args %#v", req)
	// TODO: Validate PVC's AccessMode is supported
	return &csi.ValidateVolumeCapabilitiesResponse{}, nil
}

func (d *Controller) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Controller) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Controller) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Controller) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Controller) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Controller) ControllerModifyVolume(context.Context, *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

//--- Identity service

func (c *Controller) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          "s3.csi.aws.com",
		VendorVersion: version.GetVersion().DriverVersion,
	}, nil

}

func (c *Controller) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
		},
	}, nil
}

func (c *Controller) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{
		Ready: wrapperspb.Bool(true),
	}, nil
}
