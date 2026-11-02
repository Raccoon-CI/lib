package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config says where the bucket is. Endpoint is empty for AWS itself and set
// for S3-compatible stores (MinIO, R2, Backblaze, ...).
type S3Config struct {
	Endpoint     string
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	// PathStyle addresses the bucket as host/bucket/key instead of
	// bucket.host/key. Most self-hosted stores need it.
	PathStyle bool
	// Prefix is put in front of every key.
	Prefix string
}

// S3 is a Store on an S3-compatible bucket.
type S3 struct {
	cfg     S3Config
	client  *s3.Client
	presign *s3.PresignClient
}

var _ Store = (*S3)(nil)

// NewS3 builds a store. It does no network I/O.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("blob: bucket is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("blob: access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	opts := s3.Options{
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken),
		UsePathStyle: cfg.PathStyle,
		// The SDK's default adds CRC32 trailers to every upload, which many
		// S3-compatible stores reject. Ask for checksums only when required.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	c := s3.New(opts)
	return &S3{cfg: cfg, client: c, presign: s3.NewPresignClient(c)}, nil
}

func (s *S3) key(k string) *string { return aws.String(s.cfg.Prefix + k) }

// wrap turns an SDK error into an *Error, keeping the store's error code.
func wrap(op, key string, err error) error {
	if err == nil {
		return nil
	}
	e := &Error{Op: op, Key: key, Message: err.Error()}
	var api smithy.APIError
	if errors.As(err, &api) {
		e.Code, e.Message = api.ErrorCode(), api.ErrorMessage()
	}
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		e.Status = re.HTTPStatusCode()
	}
	// HEAD has no body, so the SDK sees only the status.
	if e.Status == 404 && e.Code == "" {
		e.Code = "NotFound"
	}
	return e
}

func (s *S3) BeginUpload(ctx context.Context, key, contentType string) (Upload, error) {
	in := &s3.CreateMultipartUploadInput{Bucket: &s.cfg.Bucket, Key: s.key(key)}
	if contentType != "" {
		in.ContentType = &contentType
	}
	out, err := s.client.CreateMultipartUpload(ctx, in)
	if err != nil {
		return nil, wrap("begin upload", key, err)
	}
	return &s3Upload{s: s, key: key, id: aws.ToString(out.UploadId)}, nil
}

// ResumeUpload returns a handle on an upload started earlier, for example by
// another request of the same artifact.
func (s *S3) ResumeUpload(key, id string) Upload { return &s3Upload{s: s, key: key, id: id} }

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx,
		&s3.GetObjectInput{Bucket: &s.cfg.Bucket, Key: s.key(key)},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", wrap("presign", key, err)
	}
	return req.URL, nil
}

func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.cfg.Bucket, Key: s.key(key)})
	if err != nil {
		return Info{}, wrap("stat", key, err)
	}
	return Info{Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag), ContentType: aws.ToString(out.ContentType)}, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.cfg.Bucket, Key: s.key(key)})
	return wrap("delete", key, err) // S3 answers 204 for a missing key
}

type s3Upload struct {
	s   *S3
	key string
	id  string
}

func (u *s3Upload) ID() string { return u.id }

// maxBufferedPart is the largest part PutPart holds in memory to be able to
// resend it.
const maxBufferedPart = 32 << 20

func (u *s3Upload) PutPart(ctx context.Context, n int, r io.Reader, size int64) (Part, error) {
	if n < 1 || n > MaxParts {
		return Part{}, fmt.Errorf("blob: part number %d out of range 1..%d", n, MaxParts)
	}
	// The SDK retries a failed part (a 503 from a busy store, a dropped
	// connection) only if it can rewind the body. A stream cannot be, so a
	// part up to maxBufferedPart is read into memory first; larger ones are
	// sent as they come and fail on the first error.
	body := r
	if _, seekable := r.(io.ReadSeeker); !seekable && size <= maxBufferedPart {
		b, err := io.ReadAll(io.LimitReader(r, size))
		if err != nil {
			return Part{}, wrap("read part", u.key, err)
		}
		body = bytes.NewReader(b)
	}
	out, err := u.s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:        &u.s.cfg.Bucket,
		Key:           u.s.key(u.key),
		UploadId:      &u.id,
		PartNumber:    aws.Int32(int32(n)),
		Body:          body,
		ContentLength: aws.Int64(size),
	}, func(o *s3.Options) {
		// The block is forwarded as it arrives, so its hash is not known up
		// front and the body cannot be rewound to compute it.
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	})
	if err != nil {
		return Part{}, wrap("put part", u.key, err)
	}
	return Part{Number: n, ETag: aws.ToString(out.ETag)}, nil
}

func (u *s3Upload) Complete(ctx context.Context, parts []Part) error {
	if len(parts) == 0 {
		return errors.New("blob: no parts to complete")
	}
	ps := append([]Part(nil), parts...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Number < ps[j].Number })
	cp := make([]types.CompletedPart, len(ps))
	for i, p := range ps {
		if i > 0 && p.Number == ps[i-1].Number {
			return fmt.Errorf("blob: part %d listed twice", p.Number)
		}
		cp[i] = types.CompletedPart{PartNumber: aws.Int32(int32(p.Number)), ETag: aws.String(p.ETag)}
	}
	// The SDK also turns a 200 response that carries an <Error> body into an
	// error, which S3 can send for this call.
	_, err := u.s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          &u.s.cfg.Bucket,
		Key:             u.s.key(u.key),
		UploadId:        &u.id,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: cp},
	})
	return wrap("complete upload", u.key, err)
}

func (u *s3Upload) Abort(ctx context.Context) error {
	_, err := u.s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: &u.s.cfg.Bucket, Key: u.s.key(u.key), UploadId: &u.id,
	})
	return wrap("abort upload", u.key, err)
}
