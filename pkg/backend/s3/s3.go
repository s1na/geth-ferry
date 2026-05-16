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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
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
	if os.Getenv("FERRY_S3_DEBUG") != "" {
		out = append(out, config.WithClientLogMode(aws.LogRequestWithBody|aws.LogResponse|aws.LogRetries))
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
			// We don't use aws-sdk-go-v2's manager.Uploader (its v1.30+
			// integrity protections wrap UploadPart bodies in aws-chunked
			// encoding with a CRC32 trailer, which OVH rejects with
			// "IncompleteBody"). Our manual Put writes plain bodies with
			// an explicit CRC32 *header* — but ResponseChecksumValidation
			// would still try to validate response trailers we don't set.
			s.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			s.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
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
		if isNotFound(err) {
			return backend.Object{}, fmt.Errorf("head %s: %w", full, backend.ErrNotExist)
		}
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
		if isNotFound(err) {
			return nil, fmt.Errorf("get %s: %w", full, backend.ErrNotExist)
		}
		return nil, fmt.Errorf("get %s: %w", full, err)
	}
	return out.Body, nil
}

// MultipartConcurrency is the number of UploadPart requests in flight at
// once during a single Put. Each in-flight part owns one MultipartPartSize
// buffer, so peak memory per Put is roughly MultipartConcurrency ×
// MultipartPartSize (~1.25 GiB at the defaults). Tune both down on
// memory-constrained hosts.
const MultipartConcurrency = 5

// Put returns a streaming writer backed by an S3 multipart upload that we
// drive ourselves (no manager.Uploader). Each part is sent as a fixed-size
// byte buffer with an explicit ContentLength and an inline Crc32 header;
// no aws-chunked encoding, no trailers — keeps OVH and other strict S3
// implementations happy.
//
// The returned Writer must be terminated by Close (commit) or Abort
// (discard); abandoning it leaks an in-progress multipart upload on the
// remote bucket until the bucket's lifecycle policy reaps it.
func (b *Backend) Put(ctx context.Context, key string) (backend.Writer, error) {
	full := b.fullKey(key)
	out, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &b.bucket,
		Key:    &full,
	})
	if err != nil {
		return nil, fmt.Errorf("create multipart upload %s: %w", full, err)
	}
	return &multipartWriter{
		ctx:      ctx,
		client:   b.client,
		bucket:   b.bucket,
		key:      full,
		uploadID: aws.ToString(out.UploadId),
		partSize: MultipartPartSize,
		buf:      make([]byte, 0, MultipartPartSize),
		partNum:  1,
		sem:      make(chan struct{}, MultipartConcurrency),
	}, nil
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

// multipartWriter implements backend.Writer by buffering up to partSize
// bytes and dispatching each filled buffer as an UploadPart in a worker
// goroutine. Up to MultipartConcurrency parts are in flight at once.
//
// Termination is via either Close (commit — CompleteMultipartUpload) or
// Abort (discard — AbortMultipartUpload, using a fresh context so it
// completes even when the caller's context is cancelled). Calling one
// disables the other; calling neither leaks an in-progress upload on the
// bucket.
type multipartWriter struct {
	ctx      context.Context
	client   *s3.Client
	bucket   string
	key      string
	uploadID string
	partSize int

	buf     []byte
	partNum int32

	sem chan struct{}
	wg  sync.WaitGroup

	mu     sync.Mutex
	parts  []s3types.CompletedPart
	failed error // first part-upload error wins

	done bool // true once Close or Abort has run
}

func (w *multipartWriter) Write(p []byte) (int, error) {
	if err := w.peekErr(); err != nil {
		return 0, err
	}
	written := len(p)
	for len(p) > 0 {
		room := w.partSize - len(w.buf)
		if room == 0 {
			if err := w.dispatchBuffered(); err != nil {
				return 0, err
			}
			continue
		}
		n := len(p)
		if n > room {
			n = room
		}
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
	}
	return written, nil
}

func (w *multipartWriter) peekErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

func (w *multipartWriter) recordErr(err error) {
	w.mu.Lock()
	if w.failed == nil {
		w.failed = err
	}
	w.mu.Unlock()
}

// dispatchBuffered starts an UploadPart for the current buffer and rotates
// in a fresh one. Bounded by sem.
func (w *multipartWriter) dispatchBuffered() error {
	if len(w.buf) == 0 {
		return nil
	}
	if err := w.peekErr(); err != nil {
		return err
	}

	body := w.buf
	w.buf = make([]byte, 0, w.partSize)
	pn := w.partNum
	w.partNum++

	w.sem <- struct{}{}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() { <-w.sem }()
		w.uploadOnePart(pn, body)
	}()
	return nil
}

func (w *multipartWriter) uploadOnePart(pn int32, body []byte) {
	crcSum := crc32.ChecksumIEEE(body)
	var crcBytes [4]byte
	binary.BigEndian.PutUint32(crcBytes[:], crcSum)
	crcB64 := base64.StdEncoding.EncodeToString(crcBytes[:])

	out, err := w.client.UploadPart(w.ctx, &s3.UploadPartInput{
		Bucket:        &w.bucket,
		Key:           &w.key,
		UploadId:      &w.uploadID,
		PartNumber:    aws.Int32(pn),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ChecksumCRC32: aws.String(crcB64),
	})
	if err != nil {
		w.recordErr(fmt.Errorf("upload part %d: %w", pn, err))
		return
	}
	w.mu.Lock()
	w.parts = append(w.parts, s3types.CompletedPart{
		PartNumber:    aws.Int32(pn),
		ETag:          out.ETag,
		ChecksumCRC32: aws.String(crcB64),
	})
	w.mu.Unlock()
}

// Close finalizes the multipart upload. On any error (including
// previously-recorded part-upload errors), the upload is aborted before
// returning.
func (w *multipartWriter) Close() error {
	if w.done {
		return nil
	}
	w.done = true

	// Flush whatever's still buffered as the final (potentially small) part.
	flushErr := w.dispatchBuffered()
	w.wg.Wait()

	if err := w.peekErr(); err != nil {
		w.abortRemote()
		return err
	}
	if flushErr != nil {
		w.abortRemote()
		return flushErr
	}

	// CompleteMultipartUpload requires parts in ascending PartNumber order.
	sort.Slice(w.parts, func(i, j int) bool {
		return aws.ToInt32(w.parts[i].PartNumber) < aws.ToInt32(w.parts[j].PartNumber)
	})

	if _, err := w.client.CompleteMultipartUpload(w.ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          &w.bucket,
		Key:             &w.key,
		UploadId:        &w.uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: w.parts},
	}); err != nil {
		w.abortRemote()
		return fmt.Errorf("complete multipart %s: %w", w.key, err)
	}
	return nil
}

// Abort discards any in-flight or buffered bytes and asks S3 to forget the
// multipart upload. Safe to call multiple times; safe to call when w.ctx
// is already cancelled (uses a fresh background context with a short
// timeout so the AbortMultipartUpload RPC can still reach the server).
func (w *multipartWriter) Abort() {
	if w.done {
		return
	}
	w.done = true
	// Stop accepting new dispatches via the next peekErr; mark failure so
	// any in-flight workers wind down without recording extra errors.
	w.recordErr(errAborted)
	w.wg.Wait()
	w.abortRemote()
}

var errAborted = errors.New("upload aborted")

// abortRemote tells S3 to forget the multipart upload. Best-effort: the
// caller is already on a teardown path. Uses a fresh context with a short
// timeout so an upstream cancellation (Ctrl+C) doesn't prevent cleanup.
func (w *multipartWriter) abortRemote() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = w.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   &w.bucket,
		Key:      &w.key,
		UploadId: &w.uploadID,
	})
}

// SnapshotPath joins a snapshot name and child path under the backend's
// prefix. Useful for callers that want to print a human-readable URL.
func (b *Backend) SnapshotPath(name, child string) string {
	return path.Join(b.prefix, name, child)
}
