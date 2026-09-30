package state

import (
	"context"
	"errors"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type S3 struct {
	Client *s3.Client
	Bucket string
}

func NewS3(ctx context.Context, endpoint, bucket string) (*S3, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return &S3{s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}), bucket}, nil
}

func storeError(err error) error {
	var e smithy.APIError
	if errors.As(err, &e) {
		switch e.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return ErrNotFound
		case "PreconditionFailed", "ConditionalRequestConflict":
			return ErrConflict
		}
	}
	return err
}

func (s *S3) Get(ctx context.Context, key string) (Object, error) {
	o, err := s.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.Bucket, Key: &key})
	if err != nil {
		return Object{}, storeError(err)
	}
	return Object{o.Body, aws.ToString(o.ETag), aws.ToString(o.VersionId), aws.ToInt64(o.ContentLength)}, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	pages := s3.NewListObjectsV2Paginator(s.Client, &s3.ListObjectsV2Input{Bucket: &s.Bucket, Prefix: &prefix})
	var keys []string
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
	}
	return keys, nil
}

// Checkpoint keys are immutable. Delete the physical version rather than adding
// a delete marker that would retain the superseded disk in a versioned bucket.
func (s *S3) Delete(ctx context.Context, key string) error {
	object, err := s.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.Bucket, Key: &key})
	if errors.Is(storeError(err), ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.Client.DeleteObject(
		ctx,
		&s3.DeleteObjectInput{Bucket: &s.Bucket, Key: &key, VersionId: object.VersionId},
	)
	return storeError(err)
}

func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, match string) (Object, error) {
	var ifMatch, ifNoneMatch *string
	if match == "" {
		ifNoneMatch = aws.String("*")
	} else {
		ifMatch = &match
	}
	if size <= 64<<20 {
		in := &s3.PutObjectInput{Bucket: &s.Bucket, Key: &key, Body: body, ContentLength: &size}
		in.IfMatch, in.IfNoneMatch = ifMatch, ifNoneMatch
		o, err := s.Client.PutObject(ctx, in)
		if err != nil {
			return Object{}, storeError(err)
		}
		return Object{ETag: aws.ToString(o.ETag), VersionID: aws.ToString(o.VersionId), Size: size}, nil
	}
	start, err := s.Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.Bucket, Key: &key})
	if err != nil {
		return Object{}, err
	}
	complete := false
	defer func() {
		if !complete {
			cleanup, cancel := context.WithTimeout(context.Background(), 30_000_000_000)
			defer cancel()
			_, _ = s.Client.AbortMultipartUpload(
				cleanup,
				&s3.AbortMultipartUploadInput{Bucket: &s.Bucket, Key: &key, UploadId: start.UploadId},
			)
		}
	}()
	var parts []types.CompletedPart
	const chunk = int64(64 << 20)
	for offset := int64(0); offset < size; offset += chunk {
		n := min(chunk, size-offset)
		number := int32(len(parts) + 1)
		// SectionReader permits SDK retries without retaining the disk in memory.
		reader, ok := body.(io.ReaderAt)
		if !ok {
			return Object{}, errors.New("multipart body must support ReaderAt")
		}
		o, e := s.Client.UploadPart(
			ctx,
			&s3.UploadPartInput{
				Bucket:        &s.Bucket,
				Key:           &key,
				UploadId:      start.UploadId,
				PartNumber:    &number,
				ContentLength: &n,
				Body:          io.NewSectionReader(reader, offset, n),
			},
		)
		if e != nil {
			return Object{}, e
		}
		parts = append(parts, types.CompletedPart{ETag: o.ETag, PartNumber: &number})
	}
	in := &s3.CompleteMultipartUploadInput{
		Bucket:          &s.Bucket,
		Key:             &key,
		UploadId:        start.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}
	in.IfMatch, in.IfNoneMatch = ifMatch, ifNoneMatch
	o, err := s.Client.CompleteMultipartUpload(ctx, in)
	if err != nil {
		return Object{}, storeError(err)
	}
	complete = true
	return Object{ETag: aws.ToString(o.ETag), VersionID: aws.ToString(o.VersionId), Size: size}, nil
}
