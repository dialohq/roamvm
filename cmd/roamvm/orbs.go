package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/orbs"
	"github.com/dialohq/roamvm/internal/runner"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
)

func orbDispatch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 280*time.Second)
	defer cancel()
	c, err := kubeClient(true)
	if err != nil {
		return err
	}
	config := ctrl.GetConfigOrDie()
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	d := orbs.Dispatcher{Client: c, Namespace: env("ORB_NAMESPACE", "default"), Image: os.Getenv("ORB_VM_IMAGE")}
	d.Bootstrap = func(ctx context.Context, vm *api.VirtualMachine, token, ampURL string) error {
		credentials := runner.BootstrapConfig{Token: token, AMPURL: ampURL}
		if path := os.Getenv("ORB_NIX_BUILDERS_FILE"); path != "" {
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open remote builder configuration: %w", err)
			}
			credentials.NixBuilders = new(runner.NixBuilders)
			err = json.NewDecoder(io.LimitReader(file, 32768)).Decode(credentials.NixBuilders)
			file.Close()
			if err != nil {
				return fmt.Errorf("invalid remote builder configuration")
			}
		}
		data, err := json.Marshal(credentials)
		if err != nil {
			return err
		}
		for {
			req := kube.CoreV1().RESTClient().Post().Namespace(vm.Namespace).Resource("pods").Name(vm.Status.PodName).SubResource("exec").VersionedParams(&core.PodExecOptions{
				Container: "runner", Command: []string{"roamvm", "guest-bootstrap", "/var/lib/roamvm/running/" + string(vm.UID) + "/guest-agent.sock"}, Stdin: true, Stdout: true, Stderr: true,
			}, kubescheme.ParameterCodec)
			executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
			if err != nil {
				return err
			}
			// Never forward bootstrap payloads or guest logs to the provider log.
			err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: bytes.NewReader(data), Stdout: io.Discard, Stderr: io.Discard})
			if err == nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("guest bootstrap did not become ready: %w", ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return d.Run(ctx, orbs.Job{Operation: os.Getenv("ORB_OPERATION"), ID: os.Getenv("ORB_ID"), Type: os.Getenv("ORB_TYPE"), SnapshotID: os.Getenv("ORB_SNAPSHOT_ID"), Token: os.Getenv("ORB_TOKEN"), AMPURL: env("AMP_URL", "https://ampcode.com")})
}

func guestBootstrap(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("guest-bootstrap requires one private socket path")
	}
	var credentials runner.BootstrapConfig
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 65536)).Decode(&credentials); err != nil {
		return fmt.Errorf("invalid bootstrap payload")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return runner.Bootstrap(ctx, args[0], credentials)
}
