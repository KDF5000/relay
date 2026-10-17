package controlplane

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3BlobOptions struct {
	Endpoint, AccessKey, SecretKey, SessionToken, Bucket, Prefix, Region string
	Secure                                                               bool
}

// S3BlobStore supports AWS S3 and S3-compatible object stores such as MinIO.
type S3BlobStore struct {
	client         *minio.Client
	bucket, prefix string
}

func NewS3BlobStore(options S3BlobOptions) (*S3BlobStore, error) {
	if options.Endpoint == "" || options.Bucket == "" {
		return nil, errors.New("relay blob: S3 endpoint and bucket are required")
	}
	client, err := minio.New(options.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(options.AccessKey, options.SecretKey, options.SessionToken), Secure: options.Secure, Region: options.Region})
	if err != nil {
		return nil, err
	}
	return &S3BlobStore{client: client, bucket: options.Bucket, prefix: strings.Trim(options.Prefix, "/")}, nil
}
func (s *S3BlobStore) key(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}
func (s *S3BlobStore) Put(ctx context.Context, key string, reader io.Reader) (int64, error) {
	// Unknown-length streams otherwise allocate a 528 MiB part buffer in the SDK.
	// Keep uploads sequential with an 8 MiB buffer, even for tiny artifacts.
	info, err := s.client.PutObject(ctx, s.bucket, s.key(key), reader, -1, minio.PutObjectOptions{ContentType: "application/octet-stream", PartSize: 8 << 20, NumThreads: 1})
	return info.Size, err
}
func (s *S3BlobStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	object, err := s.client.GetObject(ctx, s.bucket, s.key(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := object.Stat(); err != nil {
		object.Close()
		return nil, err
	}
	return object, nil
}
func (s *S3BlobStore) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.key(key), minio.RemoveObjectOptions{})
}
