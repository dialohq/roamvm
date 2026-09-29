package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/dialohq/roamvm/internal/state"
	"io"
	"os"
)

func main() {
	ctx := context.Background()
	s, e := state.NewS3(ctx, os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"))
	if e != nil {
		panic(e)
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
		head, err := (state.Manager{Store: s}).Read(ctx, os.Args[2])
		e = err
		if e == nil {
			e = json.NewEncoder(os.Stdout).Encode(head.Head)
		}
	case "corrupt", "restore":
		head, err := (state.Manager{Store: s}).Read(ctx, os.Args[2])
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
