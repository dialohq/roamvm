package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/dialohq/roamvm/internal/state"
	"io"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	ctx := context.Background()
	s, e := state.NewS3(ctx, os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"))
	if e != nil {
		panic(e)
	}
	var metadata state.Store = s
	if os.Getenv("STATE_BACKEND") == "kubernetes" {
		scheme := runtime.NewScheme()
		core.AddToScheme(scheme)
		c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			panic(err)
		}
		ns := os.Getenv("STATE_NAMESPACE")
		if ns == "" {
			ns = "roamvm-system"
		}
		metadata = &state.Kubernetes{Client: c, Namespace: ns, Objects: s}
	}
	mode := "init"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "init":
		_, e = s.Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.Bucket})
		if e != nil {
			_, e = s.Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.Bucket})
		}
	case "read":
		head, err := (state.Manager{Store: metadata}).Read(ctx, os.Args[2])
		e = err
		if e == nil {
			e = json.NewEncoder(os.Stdout).Encode(head.Head)
		}
	case "checkpoints":
		keys, err := s.List(ctx, "vm/"+os.Args[2]+"/overlay/")
		e = err
		if e == nil {
			e = json.NewEncoder(os.Stdout).Encode(keys)
		}
	case "corrupt", "restore":
		head, err := (state.Manager{Store: metadata}).Read(ctx, os.Args[2])
		if err != nil {
			panic(err)
		}
		key := head.Head.Checkpoint.Key
		object, err := s.Get(ctx, key)
		if err != nil {
			panic(err)
		}
		body, err := io.ReadAll(object.Body)
		object.Body.Close()
		if err != nil {
			panic(err)
		}
		if mode == "corrupt" {
			if err = os.WriteFile(os.Args[3], body, 0600); err != nil {
				panic(err)
			}
			body = []byte("injected checkpoint corruption")
		} else {
			body, err = os.ReadFile(os.Args[3])
			if err != nil {
				panic(err)
			}
		}
		_, e = s.Put(ctx, key, bytes.NewReader(body), int64(len(body)), object.ETag)
	default:
		panic("unknown lab command")
	}
	if e != nil {
		panic(e)
	}
}
