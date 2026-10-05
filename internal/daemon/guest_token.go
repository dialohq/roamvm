package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	authentication "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const GuestTokenDirectory = "/run/roamvm/guest-token"

func (s *Server) refreshGuestToken(ctx context.Context, namespace string, token *api.GuestServiceAccountToken, now time.Time) error {
	if token == nil {
		return nil
	}
	dir := s.GuestTokenDir
	if dir == "" {
		dir = GuestTokenDirectory
	}
	path := filepath.Join(dir, "token")
	if !now.Before(s.guestTokenExpiry) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if now.Before(s.guestTokenRefresh) {
		if !now.Before(s.guestTokenExpiry) {
			return fmt.Errorf("no valid guest service account token; refresh pending")
		}
		return nil
	}
	s.guestTokenRefresh = now.Add(time.Minute)
	request := &authentication.TokenRequest{Spec: authentication.TokenRequestSpec{
		Audiences: []string{token.Audience}, ExpirationSeconds: ptr.To(int64(3600)),
	}}
	account := &core.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: token.Name, Namespace: namespace}}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.Client.SubResource("token").Create(ctx, account, request); err != nil {
		log.Printf("guest token request failed: %v", err)
		return err
	}
	expiry := request.Status.ExpirationTimestamp.Time
	if request.Status.Token == "" || !expiry.After(now) {
		return fmt.Errorf("guest TokenRequest returned an empty or expired token")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".token-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(request.Status.Token)
	if err == nil {
		err = f.Chmod(0o444)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	s.guestTokenExpiry = expiry
	s.guestTokenRefresh = now.Add(expiry.Sub(now) * 4 / 5)
	return nil
}
