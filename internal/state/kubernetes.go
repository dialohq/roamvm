package state

import (
	"context"
	"errors"
	"io"
	"strings"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Kubernetes stores only ownership and the current checkpoint pointer in etcd.
// Disk bytes remain in S3. ConfigMaps deliberately outlive VM deletion.
type Kubernetes struct {
	Client    client.Client
	Namespace string
	Objects   Store
}

func (s *Kubernetes) List(ctx context.Context, prefix string) ([]string, error) {
	return s.Objects.List(ctx, prefix)
}

func (s *Kubernetes) Delete(ctx context.Context, key string) error {
	if _, head := headName(key); head {
		return errors.New("cannot delete durable ownership state")
	}
	return s.Objects.Delete(ctx, key)
}

func headName(key string) (string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "vm" || parts[1] == "" || parts[2] != "head.json" {
		return "", false
	}
	return "roamvm-" + parts[1], true
}

func (s *Kubernetes) Get(ctx context.Context, key string) (Object, error) {
	name, head := headName(key)
	if !head {
		return s.Objects.Get(ctx, key)
	}
	var cm core.ConfigMap
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, &cm)
	if apierrors.IsNotFound(err) {
		return Object{}, ErrNotFound
	}
	if err != nil {
		return Object{}, err
	}
	data, ok := cm.Data["head.json"]
	if !ok {
		return Object{}, errors.New("state ConfigMap has no head")
	}
	return Object{Body: io.NopCloser(strings.NewReader(data)), ETag: cm.ResourceVersion, Size: int64(len(data))}, nil
}

func (s *Kubernetes) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, match string) (Object, error) {
	name, head := headName(key)
	if !head {
		if match != "" {
			return Object{}, errors.New("checkpoint generations cannot be overwritten")
		}
		return s.Objects.Put(ctx, key, body, size, "")
	}
	if size > 65536 || size < 0 {
		return Object{}, errors.New("invalid state head size")
	}
	data, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil {
		return Object{}, err
	}
	if int64(len(data)) != size {
		return Object{}, errors.New("state head size mismatch")
	}
	cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace,
		ResourceVersion: match, Labels: map[string]string{"app.kubernetes.io/name": "roamvm-state"}},
		Data: map[string]string{"head.json": string(data)}}
	if match == "" {
		err = s.Client.Create(ctx, cm)
	} else {
		err = s.Client.Update(ctx, cm)
	}
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return Object{}, ErrConflict
	}
	if err != nil {
		return Object{}, err
	}
	return Object{ETag: cm.ResourceVersion, Size: size}, nil
}

// CheckObjects verifies the data path without assuming S3 provides the CAS.
func (s *Kubernetes) CheckObjects(ctx context.Context, key string) error {
	const data = "roamvm-object-store-probe"
	if _, err := s.Objects.Put(ctx, key, strings.NewReader(data), int64(len(data)), ""); err != nil {
		return err
	}
	return checkObject(ctx, s.Objects, key, data)
}
