package controller

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/version"
)

func (cs *S3ControllerServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          driverName,
		VendorVersion: version.GetVersion().DriverVersion,
	}, nil
}

func (cs *S3ControllerServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	// The external-provisioner refuses to start unless the CSI Driver advertises `CONTROLLER_SERVICE`.
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE},
				},
			},
		},
	}, nil
}

func (cs *S3ControllerServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
