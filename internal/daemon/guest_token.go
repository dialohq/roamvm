package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
)

const (
	GuestTokenDirectory       = "/run/roamvm/guest-token"
	VaultCredentialsDirectory = "/var/run/roamvm/vault"
)

func (s *Server) refreshGuestToken(ctx context.Context, token *api.GuestVaultToken, now time.Time) error {
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
			return fmt.Errorf("no valid guest Vault token; refresh pending")
		}
		return nil
	}
	s.guestTokenRefresh = now.Add(time.Minute)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	jwt, err := s.acquireVaultJWT(ctx, token)
	if err != nil {
		log.Printf("guest Vault token refresh failed: %v", err)
		return err
	}
	expiry, err := guestJWTExpiry(jwt)
	if err != nil || !expiry.After(now) || !expiry.After(time.Now()) {
		return fmt.Errorf("Vault returned a malformed or expired identity JWT")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".token-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(jwt)
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

func guestJWTExpiry(jwt string) (time.Time, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("invalid JWT")
	}
	for _, part := range parts {
		if part == "" {
			return time.Time{}, fmt.Errorf("invalid JWT")
		}
		if _, err := base64.RawURLEncoding.DecodeString(part); err != nil {
			return time.Time{}, fmt.Errorf("invalid JWT")
		}
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var metadata struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(header, &metadata); err != nil || metadata.Alg == "" || metadata.Alg == "none" {
		return time.Time{}, fmt.Errorf("invalid JWT header")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, fmt.Errorf("invalid JWT expiry")
	}
	return time.Unix(claims.Exp, 0), nil
}

func (s *Server) acquireVaultJWT(ctx context.Context, config *api.GuestVaultToken) (string, error) {
	u, err := url.Parse(config.Address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || config.AuthRole == "" || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(config.Role) {
		return "", fmt.Errorf("invalid guest Vault configuration")
	}
	dir := s.VaultCredentialsDir
	if dir == "" {
		dir = VaultCredentialsDirectory
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return "", fmt.Errorf("cannot read Vault CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return "", fmt.Errorf("invalid Vault CA")
	}
	jwt, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil || len(bytes.TrimSpace(jwt)) == 0 {
		return "", fmt.Errorf("cannot read Vault login JWT")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path, access string, body []byte, out any) error {
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(config.Address, "/")+path, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("invalid Vault request")
		}
		req.Header.Set("Content-Type", "application/json")
		if access != "" {
			req.Header.Set("X-Vault-Token", access)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("Vault request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("Vault request returned HTTP %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			return fmt.Errorf("invalid Vault response size")
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("invalid Vault response")
		}
		return nil
	}
	body, _ := json.Marshal(map[string]string{"jwt": strings.TrimSpace(string(jwt)), "role": config.AuthRole})
	var login struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := request(http.MethodPost, "/v1/auth/kubernetes/login", "", body, &login); err != nil {
		return "", err
	}
	if login.Auth.ClientToken == "" {
		return "", fmt.Errorf("Vault login returned no access token")
	}
	var minted struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := request(http.MethodGet, "/v1/identity/oidc/token/"+config.Role, login.Auth.ClientToken, nil, &minted); err != nil {
		return "", err
	}
	return minted.Data.Token, nil
}
