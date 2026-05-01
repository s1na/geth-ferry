// Package s3 implements the Backend interface against an S3-compatible
// object store. It supports endpoint override (for OVH and other
// S3-compatible providers) via the URL's `endpoint` query parameter.
//
// URL form:
//
//	s3://<bucket>/<prefix>/?endpoint=<host>&region=<region>&path_style=<true|false>
//
// Credentials come from the standard AWS chain (env vars, ~/.aws/credentials,
// instance profile, etc.). Ferry does not invent its own credential format.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/s1na/geth-ferry/pkg/backend"
)

// MultipartPartSize is the default size of each multipart upload part. With
// the S3 limit of 10 000 parts, 256 MiB caps a single object at ~2.5 TiB —
// enough for the chaindata tarball with headroom. The user can lower this
// via the SDK options for tests.
const MultipartPartSize = 256 * 1024 * 1024

// Backend is an S3-compatible Backend rooted at a bucket + key prefix.
type Backend struct {
	client *s3.Client
	bucket string
	prefix string // canonicalized: no leading slash, trailing slash if non-empty
}

// New constructs an S3 backend against an explicit bucket + prefix +
// endpoint config. Most callers should use FromURL via the registry.
func New(ctx context.Context, bucket, prefix string, opts ClientOptions) (*Backend, error) {
	cfg, err := config.LoadDefaultConfig(ctx, opts.configOptions()...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := s3.NewFromConfig(cfg, opts.serviceOptions()...)
	return &Backend{
		client: client,
		bucket: bucket,
		prefix: canonPrefix(prefix),
	}, nil
}

// ClientOptions holds the non-secret S3 connection knobs derived from the URL.
type ClientOptions struct {
	Endpoint  string
	Region    string
	PathStyle bool
}

func (o ClientOptions) configOptions() []func(*config.LoadOptions) error {
	var out []func(*config.LoadOptions) error
	if o.Region != "" {
		out = append(out, config.WithRegion(o.Region))
	}
	return out
}

func (o ClientOptions) serviceOptions() []func(*s3.Options) {
	return []func(*s3.Options){
		func(s *s3.Options) {
			if o.Endpoint != "" {
				s.BaseEndpoint = aws.String(normalizeEndpoint(o.Endpoint))
			}
			if o.PathStyle {
				s.UsePathStyle = true
			}
		},
	}
}

// FromURL parses an s3:// URL into a configured Backend. It returns the
// Backend along with the in-backend prefix that the registry should hand
// callers — for S3 this is empty because the prefix is already baked into
// the Backend struct.
func FromURL(ctx context.Context, u *url.URL) (*Backend, string, error) {
	if u.Scheme != "s3" {
		return nil, "", fmt.Errorf("s3 backend: scheme %q unsupported", u.Scheme)
	}
	if u.Host == "" {
		return nil, "", fmt.Errorf("s3 backend: bucket missing in %s", u)
	}
	q := u.Query()
	pathStyle := true // default on for S3-compatible (OVH, MinIO). Opt out for AWS.
	if v := q.Get("path_style"); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes":
			pathStyle = true
		case "0", "false", "no":
			pathStyle = false
		default:
			return nil, "", fmt.Errorf("s3 backend: invalid path_style %q", v)
		}
	}
	be, err := New(ctx, u.Host, u.Path, ClientOptions{
		Endpoint:  q.Get("endpoint"),
		Region:    q.Get("region"),
		PathStyle: pathStyle,
	})
	if err != nil {
		return nil, "", err
	}
	return be, "", nil
}

func canonPrefix(p string) string {
	p = strings.TrimLeft(p, "/")
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func normalizeEndpoint(e string) string {
	if strings.Contains(e, "://") {
		return e
	}
	return "https://" + e
}

func (b *Backend) fullKey(key string) string {
	return b.prefix + strings.TrimLeft(key, "/")
}

func (b *Backend) trimKey(key string) string {
	return strings.TrimPrefix(key, b.prefix)
}

func (b *Backend) List(ctx context.Context, prefix string) ([]backend.Object, error) {
	full := b.fullKey(prefix)
	var out []backend.Object
	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: &b.bucket,
		Prefix: &full,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", full, err)
		}
		for _, obj := range page.Contents {
			key := ""
			if obj.Key != nil {
				key = b.trimKey(*obj.Key)
			}
			etag := ""
			if obj.ETag != nil {
				etag = *obj.ETag
			}
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			o := backend.Object{
				Key:  key,
				Size: size,
				ETag: etag,
			}
			if obj.LastModified != nil {
				o.ModTime = *obj.LastModified
			}
			out = append(out, o)
		}
	}
	return out, nil
}

func (b *Backend) Stat(ctx context.Context, key string) (backend.Object, error) {
	full := b.fullKey(key)
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &b.bucket,
		Key:    &full,
	})
	if err != nil {
		return backend.Object{}, fmt.Errorf("head %s: %w", full, err)
	}
	res := backend.Object{Key: key}
	if out.ContentLength != nil {
		res.Size = *out.ContentLength
	}
	if out.ETag != nil {
		res.ETag = *out.ETag
	}
	if out.LastModified != nil {
		res.ModTime = *out.LastModified
	}
	return res, nil
}

func (b *Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full := b.fullKey(key)
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &b.bucket,
		Key:    &full,
	})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", full, err)
	}
	return out.Body, nil
}

// Put returns a streaming writer that uploads via S3 multipart upload.
// The upload is run in a background goroutine reading from a pipe; Close
// finalizes the upload and returns its outcome. If a write fails (because
// the goroutine errored), the upload error surfaces from Close.
func (b *Backend) Put(ctx context.Context, key string) (io.WriteCloser, error) {
	full := b.fullKey(key)
	pr, pw := io.Pipe()
	uploader := manager.NewUploader(b.client, func(u *manager.Uploader) {
		u.PartSize = MultipartPartSize
	})
	w := &s3Writer{pw: pw, done: make(chan error, 1)}
	go func() {
		_, err := uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: &b.bucket,
			Key:    &full,
			Body:   pr,
		})
		// Closing the pipe reader unblocks any pending Write with an error
		// (matches uploader.Upload's behavior when it returns).
		_ = pr.CloseWithError(err)
		w.done <- err
	}()
	return w, nil
}

func (b *Backend) Delete(ctx context.Context, key string) error {
	full := b.fullKey(key)
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &b.bucket,
		Key:    &full,
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete %s: %w", full, err)
	}
	return nil
}

func isNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

type s3Writer struct {
	pw     *io.PipeWriter
	done   chan error
	closed bool
}

func (w *s3Writer) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

func (w *s3Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	// Closing the pipe writer signals EOF to the uploader; it then completes
	// the multipart upload and reports back via w.done.
	if err := w.pw.Close(); err != nil {
		<-w.done
		return err
	}
	return <-w.done
}

// CloseWithError aborts the upload by sending err down the pipe to the
// uploader goroutine. The eventual upload error is returned. Used by
// callers that need to abort mid-stream.
func (w *s3Writer) CloseWithError(err error) error {
	if w.closed {
		return nil
	}
	w.closed = true
	_ = w.pw.CloseWithError(err)
	return <-w.done
}

// SnapshotPath joins a snapshot name and child path under the backend's
// prefix. Useful for callers that want to print a human-readable URL.
func (b *Backend) SnapshotPath(name, child string) string {
	return path.Join(b.prefix, name, child)
}
