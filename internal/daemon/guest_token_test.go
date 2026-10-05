package daemon

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func testJWT(expiry time.Time) string {
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiry.Unix()))) + ".c2ln"
}

func vaultFixture(t *testing.T, handler http.HandlerFunc) (*Server, *api.GuestVaultToken) {
	t.Helper()
	vault := httptest.NewTLSServer(handler)
	t.Cleanup(vault.Close)
	s := &Server{GuestTokenDir: t.TempDir(), VaultCredentialsDir: t.TempDir()}
	require.NoError(t, os.WriteFile(filepath.Join(s.VaultCredentialsDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: vault.Certificate().Raw}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.VaultCredentialsDir, "token"), []byte("projected-first"), 0o600))
	return s, &api.GuestVaultToken{Address: vault.URL, AuthRole: "cibox-workspace", Role: "ceph-rgw", CAConfigMapName: "roamvm-vault-ca"}
}

func TestGuestTokenRotationFailureExpiryAndRestart(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	calls := 0
	failure := false
	expiry := now.Add(10 * time.Minute)
	jwt := testJWT(expiry)
	projected := "projected-first"
	s, config := vaultFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/kubernetes/login":
			calls++
			require.Equal(t, http.MethodPost, r.Method)
			require.Empty(t, r.Header.Get("X-Vault-Token"))
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, map[string]string{"role": "cibox-workspace", "jwt": projected}, body)
			if failure {
				http.Error(w, "secret failure body", 403)
				return
			}
			fmt.Fprint(w, `{"auth":{"client_token":"private-access-token"}}`)
		case "/v1/identity/oidc/token/ceph-rgw":
			require.Equal(t, http.MethodGet, r.Method)
			require.Equal(t, "private-access-token", r.Header.Get("X-Vault-Token"))
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"token": jwt, "ttl": 3600}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	refresh := func(at time.Time) error { return s.refreshGuestToken(t.Context(), config, at) }
	path := filepath.Join(s.GuestTokenDir, "token")
	read := func() string { b, err := os.ReadFile(path); require.NoError(t, err); return string(b) }
	require.NoError(t, refresh(now))
	require.Equal(t, jwt, read())
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o444), info.Mode().Perm())
	require.Equal(t, now.Add(8*time.Minute), s.guestTokenRefresh, "JWT expiry, not Vault ttl, determines refresh")
	require.NoError(t, refresh(now.Add(479*time.Second)))
	require.Equal(t, 1, calls)
	old, err := os.Open(path)
	require.NoError(t, err)
	defer old.Close()
	projected = "projected-rotated"
	require.NoError(t, os.WriteFile(filepath.Join(s.VaultCredentialsDir, "token"), []byte(projected), 0o600))
	expiry = now.Add(20 * time.Minute)
	jwt = testJWT(expiry)
	require.NoError(t, refresh(now.Add(480*time.Second)))
	require.Equal(t, 2, calls)
	require.Equal(t, jwt, read())
	oldInfo, err := old.Stat()
	require.NoError(t, err)
	newInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, os.SameFile(oldInfo, newInfo))
	failure = true
	err = refresh(now.Add(18 * time.Minute))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret failure body")
	require.Equal(t, jwt, read())
	require.NoError(t, refresh(now.Add(18*time.Minute+30*time.Second)))
	require.Equal(t, 3, calls)
	require.Error(t, refresh(expiry))
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
	failure = false
	jwt = testJWT(now.Add(time.Hour))
	require.NoError(t, refresh(now.Add(21*time.Minute)))
	require.Equal(t, jwt, read())
	restarted := &Server{GuestTokenDir: s.GuestTokenDir, VaultCredentialsDir: s.VaultCredentialsDir}
	jwt = testJWT(now.Add(2 * time.Hour))
	require.NoError(t, restarted.refreshGuestToken(t.Context(), config, now.Add(22*time.Minute)))
	require.Equal(t, jwt, read())
	files, err := os.ReadDir(s.GuestTokenDir)
	require.NoError(t, err)
	require.Len(t, files, 1, "only guest JWT is exported, never access or login credentials")
}

func TestGuestTokenInitialFailureAndInvalidResponses(t *testing.T) {
	for _, name := range []string{"TLS", "HTTP", "redirect", "empty login", "empty JWT", "malformed JWT", "expired JWT", "oversized", "invalid JSON"} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			calls := 0
			s, config := vaultFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if name == "HTTP" {
					http.Error(w, "private-access-token", 500)
					return
				}
				if name == "redirect" {
					w.Header().Set("Location", "/redirect-target")
					w.WriteHeader(307)
					return
				}
				if name == "oversized" {
					fmt.Fprint(w, strings.Repeat("x", (1<<20)+1))
					return
				}
				if name == "invalid JSON" {
					fmt.Fprint(w, "private-access-token")
					return
				}
				if r.URL.Path == "/v1/auth/kubernetes/login" {
					if name == "empty login" {
						fmt.Fprint(w, `{"auth":{}}`)
						return
					}
					fmt.Fprint(w, `{"auth":{"client_token":"private-access-token"}}`)
					return
				}
				jwt := ""
				if name == "malformed JWT" {
					jwt = "malformed"
				}
				if name == "expired JWT" {
					jwt = testJWT(now.Add(-time.Minute))
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"token": jwt}})
			})
			if name == "TLS" {
				// The CA is trusted, but the certificate does not cover localhost.
				config.Address = strings.Replace(config.Address, "127.0.0.1", "localhost", 1)
			}
			err := s.refreshGuestToken(t.Context(), config, now)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private-access-token")
			if name == "TLS" {
				require.Zero(t, calls, "TLS verification must prevent sending login credentials")
			}
			if name == "redirect" {
				require.Equal(t, 1, calls)
			}
			require.Error(t, s.refreshGuestToken(t.Context(), config, now.Add(time.Second)))
			_, err = os.Stat(filepath.Join(s.GuestTokenDir, "token"))
			require.True(t, os.IsNotExist(err))
		})
	}
}
