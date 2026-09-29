package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesAPIOwnership(t *testing.T) {
	namespace := os.Getenv("TEST_KUBERNETES_NAMESPACE")
	if namespace == "" {
		t.Skip("set TEST_KUBERNETES_NAMESPACE and KUBECONFIG for an isolated test cluster")
	}
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	config.QPS = 200
	config.Burst = 100
	scheme := runtime.NewScheme()
	core.AddToScheme(scheme)
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("race-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		err := c.Delete(context.Background(), &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "roamvm-" + id, Namespace: namespace}})
		if client.IgnoreNotFound(err) != nil {
			t.Error(err)
		}
	})
	objects := &pausedUpload{Store: newMemory(), entered: make(chan struct{}), resume: make(chan struct{})}
	m := Manager{Store: &Kubernetes{Client: c, Namespace: namespace, Objects: objects}}
	ctx := context.Background()
	var wg sync.WaitGroup
	winners := make(chan Session, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := m.Acquire(ctx, id, "base", fmt.Sprintf("pod-%d", i), "node-a")
			if err == nil {
				winners <- s
			} else if !errors.Is(err, ErrOwned) && !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("expected one owner, got %d", len(winners))
	}
	old := <-winners
	path := filepath.Join(t.TempDir(), "overlay")
	if err = os.WriteFile(path, []byte("delayed old checkpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := m.Commit(ctx, old, path); result <- err }()
	<-objects.entered
	_, recoverErr := m.Recover(ctx, id, old.Head.Owner)
	next, acquireErr := m.Acquire(ctx, id, "base", "replacement", "node-b")
	close(objects.resume)
	commitErr := <-result
	if recoverErr != nil || acquireErr != nil {
		t.Fatal(recoverErr, acquireErr)
	}
	if !errors.Is(commitErr, ErrConflict) {
		t.Fatalf("old upload committed: %v", commitErr)
	}
	current, err := m.Read(ctx, id)
	if err != nil || current.ETag != next.ETag || current.Head.Owner != "replacement" || current.Head.Checkpoint != nil {
		t.Fatalf("replacement ownership changed: %+v %v", current, err)
	}
}
