package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/state"
	core "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cleanup runs in the controller, not a new dependent Pod: foreground/namespace
// deletion can kill dependent Pods or forbid their creation. Only v2 calls this,
// after all runners and upload workers have disappeared.
func (r *Reconciler) cleanup(ctx context.Context, vm *api.VirtualMachine) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	values := map[string]string{}
	var cm core.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: runtimeName(r.Scheme, "runtime")}, &cm); client.IgnoreNotFound(err) != nil {
		return err
	}
	for k, v := range cm.Data {
		values[k] = v
	}
	var secret core.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: runtimeName(r.Scheme, "object-store")}, &secret); err != nil {
		return err
	}
	for k, v := range secret.Data {
		values[k] = string(v)
	}
	if values["S3_BUCKET"] == "" {
		return fmt.Errorf("v2 cleanup requires S3_BUCKET in runtime configuration")
	}
	var options []func(*config.LoadOptions) error
	if values["AWS_ACCESS_KEY_ID"] != "" {
		if values["AWS_SECRET_ACCESS_KEY"] == "" {
			return fmt.Errorf("incomplete runtime AWS credentials")
		}
		options = append(options, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(values["AWS_ACCESS_KEY_ID"], values["AWS_SECRET_ACCESS_KEY"], values["AWS_SESSION_TOKEN"])))
	}
	if values["AWS_REGION"] != "" {
		options = append(options, config.WithRegion(values["AWS_REGION"]))
	}
	objects, err := state.NewS3(ctx, values["S3_ENDPOINT"], values["S3_BUCKET"], options...)
	if err != nil {
		return err
	}
	var store state.Store = objects
	switch values["STATE_BACKEND"] {
	case "kubernetes":
		namespace := values["STATE_NAMESPACE"]
		if namespace == "" {
			namespace = "roamvm-v2-system"
		}
		store = &state.Kubernetes{Client: r.Client, Namespace: namespace, Objects: objects}
	case "", "s3":
	default:
		return fmt.Errorf("STATE_BACKEND must be s3 or kubernetes")
	}
	return (state.Manager{Store: store}).DeleteVM(ctx, string(vm.UID))
}
