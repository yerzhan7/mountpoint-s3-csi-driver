package controller

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
)

// unixSocketPerm is the permission of the Unix socket the CSI Controller Service is served on.
// Only the owner can write and read, which is enough as the external-provisioner sidecar
// runs in the same Pod with the same user as the controller.
const unixSocketPerm = os.FileMode(0700)

// A Server serves the CSI Controller Service on a CSI endpoint.
// It implements [sigs.k8s.io/controller-runtime/pkg/manager.Runnable] to be run
// as part of the CSI Driver's controller component.
type Server struct {
	endpoint         string
	controllerServer *S3ControllerServer
	log              logr.Logger
}

// NewServer returns a new [Server] serving `controllerServer` on given `endpoint`.
func NewServer(endpoint string, controllerServer *S3ControllerServer, log logr.Logger) *Server {
	return &Server{endpoint: endpoint, controllerServer: controllerServer, log: log}
}

// Start starts serving the CSI Controller Service, and blocks until `ctx` is cancelled.
func (s *Server) Start(ctx context.Context) error {
	scheme, addr, err := util.ParseCSIEndpoint(s.endpoint)
	if err != nil {
		return err
	}

	listener, err := net.Listen(scheme, addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %q: %w", s.endpoint, err)
	}

	if scheme == "unix" {
		// See the comment in `pkg/driver.Driver.Run` on why the permissions are changed after `net.Listen`.
		if err := os.Chmod(addr, unixSocketPerm); err != nil {
			return fmt.Errorf("failed to change permissions on unix socket %q: %w", addr, err)
		}
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(s.logErr))
	csi.RegisterIdentityServer(server, s.controllerServer)
	csi.RegisterControllerServer(server, s.controllerServer)

	go func() {
		<-ctx.Done()
		s.log.Info("Stopping CSI Controller Service")
		server.GracefulStop()
	}()

	s.log.Info("Serving CSI Controller Service", "address", listener.Addr())
	return server.Serve(listener)
}

// logErr is a [grpc.UnaryServerInterceptor] logging the errors returned by the CSI Controller Service.
// Errors caused by the caller's input, e.g. a misconfigured StorageClass, are logged without a
// stack trace as they're not a fault of the CSI Driver.
func (s *Server) logErr(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err == nil {
		return resp, nil
	}

	switch status.Code(err) {
	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.FailedPrecondition, codes.OutOfRange:
		s.log.Info("gRPC request rejected", "method", info.FullMethod, "error", err.Error())
	default:
		s.log.Error(err, "gRPC error", "method", info.FullMethod)
	}

	return resp, err
}

// NeedLeaderElection returns false as the CSI Controller Service must be reachable by the
// external-provisioner sidecar of this Pod regardless of whether this Pod is the leader -
// the external-provisioner performs its own leader election.
func (s *Server) NeedLeaderElection() bool {
	return false
}
