package device

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	dp "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type registrationServer struct {
	dp.UnimplementedRegistrationServer
	register func(context.Context, *dp.RegisterRequest) (*dp.Empty, error)
}

func (s registrationServer) Register(ctx context.Context, req *dp.RegisterRequest) (*dp.Empty, error) {
	return s.register(ctx, req)
}

func TestReregisterAfterKubeletRestart(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "kubelet.sock")
	attempts := make(chan error, 8)
	startKubelet := func() *grpc.Server {
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		require.NoError(t, err)
		listener.SetUnlinkOnClose(false)
		server := grpc.NewServer()
		dp.RegisterRegistrationServer(server, registrationServer{register: func(ctx context.Context, req *dp.RegisterRequest) (*dp.Empty, error) {
			// Kubelet connects back to the advertised endpoint during registration.
			ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			conn, err := grpc.NewClient("unix://"+filepath.Join(dir, req.Endpoint), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err == nil {
				_, err = dp.NewDevicePluginClient(conn).GetDevicePluginOptions(ctx, &dp.Empty{})
				conn.Close()
			}
			attempts <- err
			return &dp.Empty{}, err
		}})
		go server.Serve(listener)
		t.Cleanup(server.Stop)
		return server
	}
	registered := func() {
		select {
		case err := <-attempts:
			require.NoError(t, err, "advertised endpoint must already be listening")
		case <-time.After(10 * time.Second):
			t.Fatal("device plugin did not register")
		}
	}
	old := startKubelet()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- (&Plugin{Slots: 1}).Run(ctx, dir) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	registered()
	// Let the initial registration finish and enter the two-second watch wait.
	time.Sleep(100 * time.Millisecond)
	// Keep the old inode allocated so this models socket replacement, not
	// inode reuse. Kubelet removes plugin endpoints when it restarts.
	require.NoError(t, os.Rename(socket, socket+".old"))
	old.Stop()
	require.NoError(t, os.Remove(filepath.Join(dir, "roamvm-kvm.sock")))
	startKubelet()
	registered()
}
