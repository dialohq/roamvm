//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/state"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type lab struct {
	t   testing.TB
	ctx context.Context
	client.Client
	kube                  *kubernetes.Clientset
	config                *rest.Config
	cluster, image, probe string
	store                 *state.S3
	state                 state.Manager
	created               []client.Object
}

func must(t testing.TB, err error) {
	t.Helper()
	require.NoError(t, err)
}

func equal(t testing.TB, description string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %v, want %v", description, got, want)
	}
}

func newLab(t testing.TB) *lab {
	t.Helper()
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := loader.Load()
	must(t, err)
	switch raw.CurrentContext {
	case "kind-roamvm-test", "kind-roamvm", "kind-roamvm-cilium", "kind-roamvm-crash-test", "roamvm-libvirt":
	default:
		if raw.CurrentContext != os.Getenv("ROAMVM_TEST_CONTEXT") || !strings.HasPrefix(raw.CurrentContext, "roamvm-libvirt-") {
			t.Fatalf("refusing context %q; use a disposable lab", raw.CurrentContext)
		}
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{}).
		ClientConfig()
	must(t, err)
	cfg.Timeout = 20 * time.Second
	// The default 5 QPS throttles our 100ms polling and subsequent mutations.
	// Keep a bounded budget with room for startup's multiple observations.
	cfg.QPS = 50
	cfg.Burst = 100
	scheme := runtime.NewScheme()
	must(t, core.AddToScheme(scheme))
	must(t, api.AddToScheme(scheme))
	must(t, networking.AddToScheme(scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	must(t, err)
	k, err := kubernetes.NewForConfig(cfg)
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	l := &lab{
		t:       t,
		ctx:     ctx,
		Client:  c,
		kube:    k,
		config:  cfg,
		cluster: strings.TrimPrefix(raw.CurrentContext, "kind-"),
		image:   os.Getenv("ROAMVM_TEST_IMAGE"),
	}
	if l.image == "" && os.Getenv("ROAMVM_TEST_EXISTING_VM") == "" {
		t.Fatal("set ROAMVM_TEST_IMAGE to the test guest digest")
	}
	return l
}

func (l *lab) storage() {
	l.t.Helper()
	if l.store != nil {
		return
	}
	if os.Getenv("S3_ENDPOINT") == "" || os.Getenv("S3_BUCKET") == "" {
		l.t.Fatal("set S3_ENDPOINT and S3_BUCKET for this lab")
	}
	var err error
	l.store, err = state.NewS3(l.ctx, os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"))
	must(l.t, err)
	l.state.Store = l.store
	if os.Getenv("STATE_BACKEND") == "kubernetes" {
		ns := os.Getenv("STATE_NAMESPACE")
		if ns == "" {
			ns = "roamvm-system"
		}
		l.state.Store = &state.Kubernetes{Client: l.Client, Namespace: ns, Objects: l.store}
	}
}

func (l *lab) wait(description string, check func() (bool, error)) {
	l.t.Helper()
	started := time.Now()
	defer func() {
		if elapsed := time.Since(started); elapsed >= time.Second {
			l.t.Logf("wait %s: %s", description, elapsed.Round(time.Millisecond))
		}
	}()
	var last error
	err := wait.PollUntilContextTimeout(
		l.ctx,
		100*time.Millisecond,
		180*time.Second,
		true,
		func(context.Context) (bool, error) {
			ok, err := check()
			last = err
			return ok && err == nil, nil
		},
	)
	if err != nil {
		l.t.Fatalf("waiting for %s: %v (last error: %v)", description, err, last)
	}
}

func meta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }

func unique(prefix string) string { return fmt.Sprintf("%s-%x", prefix, time.Now().UnixNano()) }

func (l *lab) get(obj client.Object) {
	l.t.Helper()
	must(l.t, l.Get(l.ctx, client.ObjectKeyFromObject(obj), obj))
}

func (l *lab) create(obj client.Object) {
	l.t.Helper()
	must(l.t, l.Create(l.ctx, obj))
	l.created = append(l.created, obj)
	if len(l.created) > 1 {
		return
	}
	// Register at the first fixture, preserving earlier host-restoration cleanup.
	// Kubernetes can tear down independent resources together; do not serialize
	// their termination waits. PVC protection still fences in-use volumes.
	l.t.Cleanup(func() {
		if l.t.Failed() {
			for _, obj := range l.created {
				l.t.Logf("retained %T %s for debugging", obj, obj.GetName())
			}
			return
		}
		for _, obj := range l.created {
			must(l.t, client.IgnoreNotFound(l.Delete(l.ctx, obj)))
		}
		for _, obj := range l.created {
			l.gone(obj)
		}
	})
}

func TestFixtureCleanup(t *testing.T) {
	var operations []string
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			operations = append(operations, "delete "+obj.GetName())
			return c.Delete(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			operations = append(operations, "wait "+key.Name)
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	t.Run("fixtures", func(t *testing.T) {
		t.Cleanup(func() { operations = append(operations, "restore host") })
		l := &lab{t: t, ctx: t.Context(), Client: c}
		l.create(&core.Pod{ObjectMeta: meta("probe")})
		l.create(&core.Pod{ObjectMeta: meta("runner")})
	})
	equal(t, "submit every deletion before waiting or restoring host", operations,
		[]string{"delete probe", "delete runner", "wait probe", "wait runner", "restore host"})
}

func (l *lab) gone(obj client.Object) {
	l.t.Helper()
	l.wait("deletion of "+obj.GetName(), func() (bool, error) {
		err := l.Get(l.ctx, client.ObjectKeyFromObject(obj), obj)
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
}

func (l *lab) vm(name string) *api.VirtualMachine {
	v := &api.VirtualMachine{ObjectMeta: meta(name)}
	l.get(v)
	return v
}

func (l *lab) pod(name string) *core.Pod { p := &core.Pod{ObjectMeta: meta(name)}; l.get(p); return p }

func (l *lab) spec(prefix string) *api.VirtualMachine {
	return &api.VirtualMachine{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "VirtualMachine"},
		ObjectMeta: meta(unique(prefix)),
		Spec: api.VirtualMachineSpec{
			PowerState: "Running", Image: l.image, CPUs: 2, Memory: "512Mi", ReadinessPort: 8080, ShutdownTimeoutSeconds: 120,
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("250m")},
			},
		},
	}
}

func (l *lab) power(name, power string) {
	l.t.Helper()
	must(
		l.t,
		l.Patch(
			l.ctx,
			&api.VirtualMachine{ObjectMeta: meta(name)},
			client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"spec":{"powerState":%q}}`, power))),
		),
	)
}

func (l *lab) phase(name, phase string) *api.VirtualMachine {
	l.t.Helper()
	var v *api.VirtualMachine
	l.wait(name+" phase "+phase, func() (bool, error) {
		v = l.vm(name)
		if v.Status.Phase == "RecoveryRequired" && phase != "RecoveryRequired" {
			return false, fmt.Errorf("%s: %s", v.Status.Phase, v.Status.Message)
		}
		return v.Status.Phase == phase, nil
	})
	return v
}

func (l *lab) service(v *api.VirtualMachine, kind core.ServiceType) *core.Service {
	s := &core.Service{
		ObjectMeta: meta(v.Name),
		Spec: core.ServiceSpec{
			Type:     kind,
			Selector: map[string]string{"vm.roamvm.io/name": v.Name},
			Ports: []core.ServicePort{
				{Port: v.Spec.ReadinessPort, TargetPort: intstr.FromInt32(v.Spec.ReadinessPort)},
			},
		},
	}
	l.create(s)
	return s
}

func (l *lab) networkClient() {
	if l.probe != "" {
		return
	}
	l.probe = unique("probe")
	p := &core.Pod{ObjectMeta: meta(l.probe), Spec: core.PodSpec{
		TerminationGracePeriodSeconds: ptr.To(int64(1)),
		AutomountServiceAccountToken: ptr.To(
			false,
		), NodeSelector: map[string]string{"kubernetes.io/hostname": l.cluster + "-control-plane"},
		Tolerations: []core.Toleration{
			{
				Key:      "node-role.kubernetes.io/control-plane",
				Operator: core.TolerationOpExists,
				Effect:   core.TaintEffectNoSchedule,
			},
		},
		Containers: []core.Container{
			// PID 1 must handle TERM; bare sleep otherwise waits for forced termination.
			{Name: "curl", Image: "curlimages/curl:8.17.0", Command: []string{"sh", "-c", "trap 'exit 0' TERM INT; sleep 7200 & wait"}},
		},
	}}
	p.Labels = map[string]string{"roamvm.test/client": l.probe}
	l.create(p)
}

func podReady(p *core.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == core.PodReady && c.Status == core.ConditionTrue {
			return true
		}
	}
	return false
}

// Exec stream writers can outlive StreamWithContext when it is cancelled.
// Return a copy so callers never share storage with those writers.
type execOutput struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *execOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *execOutput) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data.Bytes())
}

func TestExecOutput(t *testing.T) {
	var out execOutput
	_, err := out.Write([]byte("prefix:"))
	must(t, err)
	first := out.snapshot()
	first[0] = 'X'
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_, _ = out.Write([]byte("ab"))
		}
	}()
	for range 1000 {
		snapshot := string(out.snapshot())
		if !strings.HasPrefix(snapshot, "prefix:") || strings.TrimPrefix(snapshot, "prefix:") != strings.Repeat("ab", (len(snapshot)-7)/2) {
			t.Fatalf("corrupt output snapshot: %q", snapshot)
		}
	}
	<-done
	equal(t, "complete exec output", string(out.snapshot()), "prefix:"+strings.Repeat("ab", 1000))
}

func (l *lab) exec(pod, container string, input []byte, args ...string) ([]byte, error) {
	req := l.kube.CoreV1().
		RESTClient().
		Post().
		Namespace("default").
		Resource("pods").
		Name(pod).
		SubResource("exec").
		VersionedParams(&core.PodExecOptions{Container: container, Command: args, Stdin: input != nil, Stdout: true, Stderr: true}, runtime.NewParameterCodec(l.Scheme()))
	executor, err := remotecommand.NewSPDYExecutor(l.config, "POST", req.URL())
	if err != nil {
		return nil, err
	}
	var out, stderr execOutput
	ctx, cancel := context.WithTimeout(l.ctx, 45*time.Second)
	defer cancel()
	options := remotecommand.StreamOptions{Stdout: &out, Stderr: &stderr}
	if input != nil {
		options.Stdin = bytes.NewReader(input)
	}
	err = executor.StreamWithContext(ctx, options)
	if err != nil {
		return out.snapshot(), fmt.Errorf("exec %s/%s: %w: %s", pod, container, err, stderr.snapshot())
	}
	return out.snapshot(), nil
}

func (l *lab) http(name, path string, body []byte, options ...string) ([]byte, error) {
	args := []string{"curl", "-fsS", "--max-time", "20"}
	if body != nil {
		args = append(args, "--data-binary", "@-")
	}
	args = append(args, options...)
	args = append(args, "http://"+name+".default.svc.cluster.local:8080"+path)
	return l.exec(l.probe, "curl", body, args...)
}

func (l *lab) request(name, path string, body []byte) []byte {
	l.t.Helper()
	data, err := l.http(name, path, body)
	must(l.t, err)
	return data
}

func (l *lab) ready(name string) *api.VirtualMachine {
	l.t.Helper()
	v := l.phase(name, "Running")
	// Provision the probe and VM together; both must be ready before assertions.
	l.wait("network probe", func() (bool, error) { return podReady(l.pod(l.probe)), nil })
	l.wait(
		"guest HTTP response",
		func() (bool, error) {
			// Pod readiness can precede Service routing. Retry a dropped SYN
			// at the polling cadence, retaining the overall readiness deadline.
			b, e := l.http(name, "/ready", nil, "--connect-timeout", "0.1")
			return string(b) == "ready\n", e
		},
	)
	return v
}

func (l *lab) start(name string) *api.VirtualMachine {
	l.t.Helper()
	l.power(name, "Running")
	return l.ready(name)
}

func (l *lab) head(v *api.VirtualMachine) state.Head {
	l.t.Helper()
	l.storage()
	s, err := l.state.Read(l.ctx, string(v.UID))
	must(l.t, err)
	return s.Head
}

func (l *lab) stop(name string) state.Head {
	l.t.Helper()
	previous := l.vm(name).Status.PodName
	l.power(name, "Stopped")
	v := l.phase(name, "Stopped")
	l.wait(name+" checkpoint durable", func() (bool, error) {
		v = l.vm(name)
		condition := apimeta.FindStatusCondition(v.Status.Conditions, "CheckpointReady")
		return v.Status.Phase == "Stopped" && condition != nil &&
			condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == v.Generation, nil
	})
	h := l.head(v)
	equal(l.t, "durable stopped state", h.State, "Stopped")
	equal(l.t, "released owner", h.Owner, "")
	if h.Checkpoint == nil {
		l.t.Fatal("missing durable checkpoint")
	}
	equal(l.t, "Kubernetes checkpoint", v.Status.Checkpoint, h.Checkpoint)
	keys, err := l.store.List(l.ctx, "vm/"+string(v.UID)+"/overlay/")
	must(l.t, err)
	equal(l.t, "only current checkpoint retained", keys, []string{h.Checkpoint.Key})
	if previous != "" {
		l.gone(&core.Pod{ObjectMeta: meta(previous)})
	}
	if v.Status.Local == nil || !v.Status.Local.Durable {
		l.t.Fatal("missing retained local checkpoint")
	}
	l.get(&core.PersistentVolumeClaim{ObjectMeta: meta(v.Status.Local.ClaimName)})
	return h
}

func command(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return data, fmt.Errorf("%s: %w: %s", args[0], err, stderr.String())
	}
	return data, nil
}

func (l *lab) run(input []byte, args ...string) []byte {
	l.t.Helper()
	b, e := command(l.ctx, input, args...)
	must(l.t, e)
	return b
}

func (l *lab) recover(v *api.VirtualMachine, owner string) {
	cli := os.Getenv("ROAMVM_TEST_CLI")
	if cli == "" {
		cli = "../../bin/roamvm"
	}
	l.run(nil, cli, "recover", "--vm-id", string(v.UID), "--owner", owner, "--fenced")
	l.phase(v.Name, "Stopped")
}

func (l *lab) cordon(name string, value bool) {
	l.t.Helper()
	must(
		l.t,
		l.Patch(
			l.ctx,
			&core.Node{ObjectMeta: metav1.ObjectMeta{Name: name}},
			client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, value))),
		),
	)
}

func (l *lab) exclusiveNode(node, permittedVM string) {
	l.t.Helper()
	if !strings.HasPrefix(node, l.cluster+"-worker") {
		l.t.Fatalf("not a lab worker: %s", node)
	}
	var vms api.VirtualMachineList
	must(l.t, l.List(l.ctx, &vms))
	for _, v := range vms.Items {
		if v.Status.NodeName == node && v.Name != permittedVM {
			l.t.Fatalf("worker %s has another VM: %s/%s", node, v.Namespace, v.Name)
		}
	}
}

func (l *lab) forward(pod string, port int32) (string, func()) {
	l.t.Helper()
	transport, upgrader, err := spdy.RoundTripperFor(l.config)
	must(l.t, err)
	url := l.kube.CoreV1().
		RESTClient().
		Post().
		Namespace("default").
		Resource("pods").
		Name(pod).
		SubResource("portforward").
		URL()
	stop, ready := make(chan struct{}), make(chan struct{})
	f, err := portforward.NewOnAddresses(
		spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url),
		[]string{"127.0.0.1"},
		[]string{fmt.Sprintf("0:%d", port)},
		stop,
		ready,
		io.Discard,
		io.Discard,
	)
	must(l.t, err)
	done := make(chan error, 1)
	go func() { done <- f.ForwardPorts() }()
	var closeOnce bool
	cleanup := func() {
		if !closeOnce {
			close(stop)
			closeOnce = true
			<-done
		}
	}
	select {
	case <-ready:
	case err := <-done:
		close(stop)
		l.t.Fatalf("port forward exited: %v", err)
	case <-time.After(20 * time.Second):
		cleanup()
		l.t.Fatal("port forward readiness timeout")
	}
	ports, err := f.GetPorts()
	must(l.t, err)
	l.t.Cleanup(cleanup)
	return fmt.Sprintf("127.0.0.1:%d", ports[0].Local), cleanup
}
