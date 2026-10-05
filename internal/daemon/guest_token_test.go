package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/stretchr/testify/require"
	authentication "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestGuestTokenRotationFailureExpiryAndRestart(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	calls := 0
	failure := false
	jwt := "first-jwt"
	expiry := now.Add(10 * time.Minute)
	s := &Server{GuestTokenDir: t.TempDir()}
	s.Client = fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceCreate: func(_ context.Context, _ client.Client, subresource string, obj, body client.Object, _ ...client.SubResourceCreateOption) error {
			calls++
			require.Equal(t, "token", subresource)
			require.Equal(t, "workspace-a", obj.GetNamespace())
			require.Equal(t, "guest-a", obj.GetName())
			request := body.(*authentication.TokenRequest)
			require.Equal(t, []string{"ceph-rgw"}, request.Spec.Audiences)
			require.EqualValues(t, 3600, *request.Spec.ExpirationSeconds)
			require.Nil(t, request.Spec.BoundObjectRef)
			if failure {
				return errors.New("API unavailable")
			}
			request.Status = authentication.TokenRequestStatus{Token: jwt, ExpirationTimestamp: metav1.NewTime(expiry)}
			return nil
		},
	}).Build()
	config := &api.GuestServiceAccountToken{Name: "guest-a", Audience: "ceph-rgw"}
	refresh := func(at time.Time) error { return s.refreshGuestToken(t.Context(), "workspace-a", config, at) }
	path := filepath.Join(s.GuestTokenDir, "token")
	read := func() string {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		return string(b)
	}
	require.NoError(t, refresh(now))
	require.Equal(t, "first-jwt", read())
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o444), info.Mode().Perm())
	require.NoError(t, refresh(now.Add(479*time.Second)))
	require.Equal(t, 1, calls)
	old, err := os.Open(path)
	require.NoError(t, err)
	defer old.Close()
	jwt, expiry = "rotated-jwt", now.Add(20*time.Minute)
	require.NoError(t, refresh(now.Add(480*time.Second)))
	require.Equal(t, 2, calls)
	require.Equal(t, "rotated-jwt", read())
	oldInfo, err := old.Stat()
	require.NoError(t, err)
	newInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, os.SameFile(oldInfo, newInfo), "rotation must replace the file atomically")

	failure = true
	require.Error(t, refresh(now.Add(18*time.Minute)))
	require.Equal(t, "rotated-jwt", read())
	require.NoError(t, refresh(now.Add(18*time.Minute+30*time.Second)))
	require.Equal(t, 3, calls, "failed requests must be throttled")
	require.Error(t, refresh(expiry))
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "expired tokens must be removed")
	failure, jwt, expiry = false, "recovered-jwt", now.Add(time.Hour)
	require.NoError(t, refresh(now.Add(21*time.Minute)))
	require.Equal(t, "recovered-jwt", read())
	partial, err := filepath.Glob(filepath.Join(s.GuestTokenDir, ".token-*"))
	require.NoError(t, err)
	require.Empty(t, partial)

	restarted := &Server{Client: s.Client, GuestTokenDir: s.GuestTokenDir}
	jwt = "restart-jwt"
	require.NoError(t, restarted.refreshGuestToken(t.Context(), "workspace-a", config, now.Add(22*time.Minute)))
	require.Equal(t, "restart-jwt", read())
}

func TestGuestTokenInitialFailureAndInvalidResponses(t *testing.T) {
	for _, name := range []string{"API failure", "empty token", "expired token"} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			s := &Server{GuestTokenDir: t.TempDir()}
			s.Client = fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				SubResourceCreate: func(_ context.Context, _ client.Client, _ string, _, body client.Object, _ ...client.SubResourceCreateOption) error {
					if name == "API failure" {
						return errors.New("denied")
					}
					response := body.(*authentication.TokenRequest)
					response.Status.ExpirationTimestamp = metav1.NewTime(now.Add(time.Hour))
					if name == "expired token" {
						response.Status.Token = "expired"
						response.Status.ExpirationTimestamp = metav1.NewTime(now)
					}
					return nil
				},
			}).Build()
			config := &api.GuestServiceAccountToken{Name: "guest", Audience: "rgw"}
			require.Error(t, s.refreshGuestToken(t.Context(), "workspace", config, now))
			require.Error(t, s.refreshGuestToken(t.Context(), "workspace", config, now.Add(time.Second)))
			_, err := os.Stat(filepath.Join(s.GuestTokenDir, "token"))
			require.True(t, os.IsNotExist(err))
		})
	}
}
