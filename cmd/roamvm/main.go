package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/device"
	"github.com/dialohq/roamvm/internal/images"
	"github.com/dialohq/roamvm/internal/runner"
	"github.com/dialohq/roamvm/internal/state"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	core "k8s.io/api/core/v1"
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
func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = core.AddToScheme(s)
	_ = api.AddToScheme(s)
	return s
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: roamvm controller|daemon|runner|device-plugin|start|stop|image-push|state|recover")
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	ctx := ctrl.SetupSignalHandler()
	switch os.Args[1] {
	case "runner":
		return runner.Run()
	case "device-plugin":
		return (&device.Plugin{Slots: 1024}).Run(ctx, "/var/lib/kubelet/device-plugins")
	case "controller":
		m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme(), LeaderElection: true, LeaderElectionID: "roamvm-controller", LeaderElectionNamespace: env("POD_NAMESPACE", "roamvm-system"), Metrics: metrics.Options{BindAddress: ":8080"}, HealthProbeBindAddress: ":8081"})
		if err != nil {
			return err
		}
		r := &controller.Reconciler{Client: m.GetClient(), Scheme: m.GetScheme(), Image: env("RUNNER_IMAGE", "roamvm:dev"), Root: env("RUNTIME_ROOT", "/var/lib/roamvm")}
		if err = r.Setup(m); err != nil {
			return err
		}
		_ = m.AddHealthzCheck("healthz", healthz.Ping)
		_ = m.AddReadyzCheck("readyz", healthz.Ping)
		return m.Start(ctx)
	case "daemon":
		c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme()})
		if err != nil {
			return err
		}
		store, err := state.NewS3(ctx, os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"))
		if err != nil {
			return err
		}
		root := env("RUNTIME_ROOT", "/var/lib/roamvm")
		server := &daemon.Server{Client: c, Node: os.Getenv("NODE_NAME"), Root: root, State: state.Manager{Store: store}, Cache: images.Cache{Root: root + "/images", PlainHTTP: os.Getenv("REGISTRY_PLAIN_HTTP") == "true"}}
		if server.Node == "" || store.Bucket == "" {
			return errors.New("NODE_NAME and S3_BUCKET are required")
		}
		if err = store.CheckSemantics(ctx); err != nil {
			return fmt.Errorf("unsafe or unavailable object store: %w", err)
		}
		return server.Serve(ctx, "/run/roamvm/runtime.sock")
	case "start", "stop":
		f := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
		ns := f.String("namespace", "default", "VM namespace")
		if err := f.Parse(os.Args[2:]); err != nil {
			return err
		}
		if f.NArg() != 1 {
			return errors.New("specify one VM name")
		}
		c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme()})
		if err != nil {
			return err
		}
		var vm api.VirtualMachine
		if err = c.Get(ctx, types.NamespacedName{Namespace: *ns, Name: f.Arg(0)}, &vm); err != nil {
			return err
		}
		before := vm.DeepCopy()
		vm.Spec.PowerState = "Running"
		if os.Args[1] == "stop" {
			vm.Spec.PowerState = "Stopped"
		}
		return c.Patch(ctx, &vm, client.MergeFrom(before))
	case "image-push":
		return push(ctx, os.Args[2:])
	case "state", "recover":
		f := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
		id := f.String("vm-id", "", "Kubernetes VM UID")
		owner := f.String("owner", "", "exact previous Pod UID")
		fenced := f.Bool("fenced", false, "confirm the previous VMM is terminated or its node physically fenced")
		if err := f.Parse(os.Args[2:]); err != nil {
			return err
		}
		if *id == "" {
			return errors.New("--vm-id is required")
		}
		store, err := state.NewS3(ctx, os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"))
		if err != nil {
			return err
		}
		m := state.Manager{Store: store}
		var session state.Session
		if os.Args[1] == "recover" {
			if !*fenced || *owner == "" {
				return errors.New("recovery requires --fenced and the exact --owner; a timeout is not fencing")
			}
			c, e := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme()})
			if e != nil {
				return e
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
		return fmt.Errorf("unknown command %q", os.Args[1])
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
