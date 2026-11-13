package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/device"
	"github.com/dialohq/roamvm/internal/runner"
	"github.com/dialohq/roamvm/internal/state"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"golang.org/x/sync/errgroup"
	authentication "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func scheme(v2 bool) *runtime.Scheme {
	s := runtime.NewScheme()
	_ = core.AddToScheme(s)
	_ = authentication.AddToScheme(s)
	if v2 {
		_ = api.AddV2ToScheme(s)
	} else {
		_ = api.AddToScheme(s)
	}
	return s
}

func kubeClient(v2 bool) (client.Client, error) {
	return client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme(v2)})
}

func run() error {
	args := os.Args[1:]
	v2 := len(args) > 0 && args[0] == "--experimental-v2"
	if v2 {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("usage: roamvm [--experimental-v2] controller|daemon|checkpoint|checkpoint-worker|runner|device-plugin|start|stop|image-push|state|recover")
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	ctx := ctrl.SetupSignalHandler()
	prefix := "roamvm"
	if v2 {
		prefix = "roamvm-v2"
	}
	switch args[0] {
	case "orb-dispatch":
		if !v2 {
			return errors.New("orb-dispatch requires --experimental-v2")
		}
		return orbDispatch(ctx)
	case "guest-bootstrap":
		return guestBootstrap(ctx, args[1:])
	case "runner":
		return runner.Run()
	case "device-plugin":
		if !v2 {
			return (&device.Plugin{Slots: 1024}).Run(ctx, "/var/lib/kubelet/device-plugins")
		}
		group, ctx := errgroup.WithContext(ctx)
		group.Go(func() error { return (&device.Plugin{Slots: 1024}).Run(ctx, "/var/lib/kubelet/device-plugins") })
		group.Go(func() error {
			return (&device.Plugin{Slots: 1024, Fuse: true}).Run(ctx, "/var/lib/kubelet/device-plugins")
		})
		return group.Wait()
	case "controller":
		warmTTL := "0s"
		if v2 {
			warmTTL = "1h"
		}
		ttl, err := time.ParseDuration(env("WARM_CACHE_TTL", warmTTL))
		if err != nil || ttl < 0 {
			return errors.New("WARM_CACHE_TTL must be a nonnegative duration")
		}
		budget, err := resource.ParseQuantity(env("WARM_CACHE_SIZE", "0"))
		if err != nil || budget.Sign() < 0 {
			return errors.New("WARM_CACHE_SIZE must be a nonnegative storage quantity")
		}
		m, err := ctrl.NewManager(
			ctrl.GetConfigOrDie(),
			ctrl.Options{
				Scheme:                  scheme(v2),
				Client:                  client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&core.Secret{}, &core.ConfigMap{}}}},
				LeaderElection:          true,
				LeaderElectionID:        prefix + "-controller",
				LeaderElectionNamespace: env("POD_NAMESPACE", prefix+"-system"),
				Metrics:                 metrics.Options{BindAddress: ":8080"},
				HealthProbeBindAddress:  ":8081",
			},
		)
		if err != nil {
			return err
		}
		r := &controller.Reconciler{
			Client:       m.GetClient(),
			Scheme:       m.GetScheme(),
			Image:        env("RUNNER_IMAGE", "roamvm:dev"),
			StorageClass: os.Getenv("WORKING_STORAGE_CLASS"),
			StorageSize:  env("WORKING_STORAGE_SIZE", "64Gi"),
			WarmTTL:      ttl,
			WarmBytes:    budget.Value(),
		}
		if err = r.Setup(m); err != nil {
			return err
		}
		_ = m.AddHealthzCheck("healthz", healthz.Ping)
		_ = m.AddReadyzCheck("readyz", healthz.Ping)
		return m.Start(ctx)
	case "daemon", "checkpoint", "checkpoint-worker", "clone-worker":
		c, err := kubeClient(v2)
		if err != nil {
			return err
		}
		store, err := state.NewS3(ctx, os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"))
		if err != nil {
			return err
		}
		root := env("RUNTIME_ROOT", "/var/lib/roamvm")
		backend, err := metadataStore(store, c)
		if err != nil {
			return err
		}
		server := &daemon.Server{
			Client:  c,
			Node:    os.Getenv("NODE_NAME"),
			PodUID:  os.Getenv("POD_UID"),
			VMUID:   os.Getenv("VM_UID"),
			Pod:     types.NamespacedName{Namespace: os.Getenv("POD_NAMESPACE"), Name: os.Getenv("POD_NAME")},
			Root:    root,
			State:   state.Manager{Store: backend, Shared: v2},
			BaseDir: "/base/disk",
		}
		if server.Node == "" || server.PodUID == "" || server.VMUID == "" || server.Pod.Name == "" || server.Pod.Namespace == "" || store.Bucket == "" {
			return errors.New("NODE_NAME, POD_UID, VM_UID, POD_NAME, POD_NAMESPACE and S3_BUCKET are required")
		}
		// Local stop and same-node resume do not need the object data path.
		// Validate it in the independent upload process instead of blocking boot.
		if args[0] != "daemon" {
			if kubernetes, ok := backend.(*state.Kubernetes); ok {
				probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				err = kubernetes.CheckObjects(probeCtx, "runtime-probes/"+rand.Text())
				cancel()
			} else {
				err = store.CheckSemantics(ctx)
			}
			if err != nil {
				return fmt.Errorf("unsafe or unavailable object store: %w", err)
			}
		}
		if args[0] == "clone-worker" {
			return server.CloneDisk(ctx)
		}
		if args[0] == "checkpoint-worker" {
			return server.CheckpointLocal(ctx)
		}
		if args[0] == "checkpoint" {
			return server.CheckpointTerminatedRunner(ctx)
		}
		return server.Serve(ctx, "/run/roamvm/runtime.sock")
	case "start", "stop":
		f := flag.NewFlagSet(args[0], flag.ContinueOnError)
		ns := f.String("namespace", "default", "VM namespace")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 1 {
			return errors.New("specify one VM name")
		}
		c, err := kubeClient(v2)
		if err != nil {
			return err
		}
		var vm api.VirtualMachine
		if err = c.Get(ctx, types.NamespacedName{Namespace: *ns, Name: f.Arg(0)}, &vm); err != nil {
			return err
		}
		before := vm.DeepCopy()
		vm.Spec.PowerState = "Running"
		if args[0] == "stop" {
			vm.Spec.PowerState = "Stopped"
		}
		return c.Patch(ctx, &vm, client.MergeFrom(before))
	case "image-push":
		return push(ctx, args[1:])
	case "state", "recover":
		f := flag.NewFlagSet(args[0], flag.ContinueOnError)
		id := f.String("vm-id", "", "Kubernetes VM UID")
		owner := f.String("owner", "", "exact previous Pod UID")
		fenced := f.Bool("fenced", false, "confirm the previous VMM is terminated or its node physically fenced")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" {
			return errors.New("--vm-id is required")
		}
		store, err := state.NewS3(ctx, os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"))
		if err != nil {
			return err
		}
		var c client.Client
		if args[0] == "recover" || os.Getenv("STATE_BACKEND") == "kubernetes" {
			c, err = kubeClient(v2)
			if err != nil {
				return err
			}
		}
		backend, err := metadataStore(store, c)
		if err != nil {
			return err
		}
		m := state.Manager{Store: backend}
		var session state.Session
		if args[0] == "recover" {
			if !*fenced || *owner == "" {
				return errors.New("recovery requires --fenced and the exact --owner; a timeout is not fencing")
			}
			session, err = controller.Recover(ctx, c, m, *id, *owner)
		} else {
			session, err = m.Read(ctx, *id)
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(session.Head)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func metadataStore(objects state.Store, c client.Client) (state.Store, error) {
	switch env("STATE_BACKEND", "s3") {
	case "s3":
		return objects, nil
	case "kubernetes":
		if c == nil {
			return nil, errors.New("Kubernetes metadata requires a Kubernetes client")
		}
		namespace := "roamvm-system"
		if api.IsV2(c.Scheme()) {
			namespace = "roamvm-v2-system"
		}
		return &state.Kubernetes{Client: c, Namespace: env("STATE_NAMESPACE", namespace), Objects: objects}, nil
	default:
		return nil, errors.New("STATE_BACKEND must be s3 or kubernetes")
	}
}

func push(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("image-push", flag.ContinueOnError)
	tag := f.String("tag", "", "registry image tag")
	path := f.String("tar", "", "tar containing disk/manifest.json and disk files")
	plain := f.Bool("plain-http", false, "allow HTTP registry for a local lab")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *tag == "" || *path == "" {
		return errors.New("--tag and --tar are required")
	}
	options := []name.Option{}
	if *plain {
		options = append(options, name.Insecure)
	}
	ref, err := name.NewTag(*tag, options...)
	if err != nil {
		return err
	}
	layer, err := tarball.LayerFromFile(*path)
	if err != nil {
		return err
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return err
	}
	if err = remote.Write(ref, img, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
		return err
	}
	digest, err := img.Digest()
	if err != nil {
		return err
	}
	fmt.Println(ref.Context().Digest(digest.String()).Name())
	return nil
}
