// Package device exposes shareable KVM/TUN access through kubelet's device API.
// The count is an admission ceiling; CPU and RAM remain ordinary Pod resources.
package device

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	dp "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type Plugin struct {
	dp.UnimplementedDevicePluginServer
	Slots int
}

func (p *Plugin) GetDevicePluginOptions(context.Context, *dp.Empty) (*dp.DevicePluginOptions, error) {
	return &dp.DevicePluginOptions{}, nil
}

func (p *Plugin) ListAndWatch(_ *dp.Empty, stream dp.DevicePlugin_ListAndWatchServer) error {
	for {
		health := dp.Healthy
		if _, err := os.Stat("/dev/kvm"); err != nil {
			health = dp.Unhealthy
		}
		if _, err := os.Stat("/dev/net/tun"); err != nil {
			health = dp.Unhealthy
		}
		devices := make([]*dp.Device, p.Slots)
		for i := range devices {
			devices[i] = &dp.Device{ID: strconv.Itoa(i), Health: health}
		}
		if err := stream.Send(&dp.ListAndWatchResponse{Devices: devices}); err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *Plugin) Allocate(_ context.Context, in *dp.AllocateRequest) (*dp.AllocateResponse, error) {
	out := &dp.AllocateResponse{}
	for _, req := range in.ContainerRequests {
		if len(req.DevicesIds) != 1 {
			return nil, errors.New("request exactly one KVM slot")
		}
		n, e := strconv.Atoi(req.DevicesIds[0])
		if e != nil || n < 0 || n >= p.Slots {
			return nil, errors.New("invalid KVM slot")
		}
		out.ContainerResponses = append(out.ContainerResponses, &dp.ContainerAllocateResponse{Devices: []*dp.DeviceSpec{
			{HostPath: "/dev/kvm", ContainerPath: "/dev/kvm", Permissions: "rw"},
			{HostPath: "/dev/net/tun", ContainerPath: "/dev/net/tun", Permissions: "rw"},
		}})
	}
	return out, nil
}

func (p *Plugin) PreStartContainer(
	context.Context,
	*dp.PreStartContainerRequest,
) (*dp.PreStartContainerResponse, error) {
	return &dp.PreStartContainerResponse{}, nil
}

func (p *Plugin) GetPreferredAllocation(
	context.Context,
	*dp.PreferredAllocationRequest,
) (*dp.PreferredAllocationResponse, error) {
	return &dp.PreferredAllocationResponse{}, nil
}

func (p *Plugin) Run(ctx context.Context, dir string) error {
	socket := filepath.Join(dir, "roamvm-kvm.sock")
	for ctx.Err() == nil {
		_ = os.Remove(socket)
		l, err := net.Listen("unix", socket)
		if err != nil {
			return err
		}
		server := grpc.NewServer()
		dp.RegisterDevicePluginServer(server, p)
		go server.Serve(l)
		var identity os.FileInfo
		for ctx.Err() == nil {
			current, e := os.Stat(filepath.Join(dir, "kubelet.sock"))
			if e == nil && (identity == nil || !os.SameFile(current, identity)) {
				regctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				conn, e := grpc.NewClient(
					"unix://"+filepath.Join(dir, "kubelet.sock"),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if e == nil {
					_, e = dp.NewRegistrationClient(conn).
						Register(regctx, &dp.RegisterRequest{Version: dp.Version, Endpoint: filepath.Base(socket), ResourceName: "vm.roamvm.io/kvm"})
					conn.Close()
				}
				cancel()
				if e == nil {
					identity = current
				} else {
					fmt.Fprintln(os.Stderr, "device registration:", e)
				}
			}
			if _, e = os.Stat(socket); os.IsNotExist(e) {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
		server.Stop()
		l.Close()
	}
	return nil
}
