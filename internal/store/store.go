// Package store wraps the S3 API surface the contract covers (PUT, GET, LIST,
// DELETE, conditional PUT) with outcome classification and latency recording.
//
// SDK auto-retry is disabled: the suite's verdicts are outcome-based, and a
// silent replay of a conditional write would corrupt the observed history.
// Every operation returns an Outcome; transport-level failures on writes are
// classified Ambiguous (the request may or may not have been applied) and must
// be resolved by the caller via read-back.
package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Outcome classifies the result of one operation.
type Outcome string

const (
	// OK: the store acknowledged the operation.
	OK Outcome = "ok"
	// PreconditionFailed: a conditional write was rejected with 412.
	PreconditionFailed Outcome = "precondition_failed"
	// NotFound: the key does not exist (definite answer from the store).
	NotFound Outcome = "not_found"
	// Ambiguous: the request may or may not have been applied (transport
	// failure, timeout, or 5xx). Resolve by read-back.
	Ambiguous Outcome = "ambiguous"
	// Failed: a definite error that cannot have mutated state ambiguously
	// (4xx other than 404/412).
	Failed Outcome = "failed"
)

// Store is one client vantage onto the bucket under test.
type Store struct {
	Vantage string // label recorded with every operation (e.g. region or endpoint host)
	Bucket  string
	Proxy   string // proxy URL this vantage routes through, if any

	client       *s3.Client
	rec          *Recorder
	timeout      time.Duration
	regionHeader string
	regions      regionTally
}

// Options configures a Store.
type Options struct {
	Endpoint     string
	Region       string
	Bucket       string
	Vantage      string
	PathStyle    bool
	Timeout      time.Duration // per-operation timeout; 0 means 30s
	ProxyURL     string        // route this vantage through a proxy (socks5://, http://, https://)
	RegionHeader string        // response header naming the serving region; "" disables capture
}

// New builds a Store using the standard AWS credential chain.
func New(ctx context.Context, opts Options, rec *Recorder) (*Store, error) {
	httpClient, err := buildHTTPClient(opts.ProxyURL, opts.RegionHeader)
	if err != nil {
		return nil, err
	}
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(opts.Region),
		config.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(opts.Endpoint)
		o.UsePathStyle = opts.PathStyle
		o.Retryer = aws.NopRetryer{}
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	return &Store{
		Vantage:      opts.Vantage,
		Bucket:       opts.Bucket,
		Proxy:        opts.ProxyURL,
		client:       client,
		rec:          rec,
		timeout:      opts.Timeout,
		regionHeader: opts.RegionHeader,
	}, nil
}

// RegionsObserved returns serving-region counts seen at this vantage.
func (s *Store) RegionsObserved() map[string]int64 {
	return s.regions.snapshot()
}

func (s *Store) op(ctx context.Context, name string, fn func(context.Context) error) error {
	opCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	var rc *RegionCapture
	if s.regionHeader != "" {
		// An outer capture (e.g. from a test group annotating its history)
		// takes precedence; otherwise attach our own for the tally.
		if rc = captureFrom(opCtx); rc == nil {
			opCtx, rc = ContextWithRegionCapture(opCtx)
		}
	}
	start := time.Now()
	err := fn(opCtx)
	s.rec.Observe(name, time.Since(start))
	if rc != nil {
		s.regions.add(rc.Value())
	}
	return err
}

// Put writes unconditionally. Returns the new ETag on OK.
func (s *Store) Put(ctx context.Context, key string, body []byte) (string, Outcome) {
	var etag string
	err := s.op(ctx, "PUT", func(c context.Context) error {
		out, err := s.client.PutObject(c, &s3.PutObjectInput{
			Bucket: aws.String(s.Bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(body),
		})
		if err == nil {
			etag = aws.ToString(out.ETag)
		}
		return err
	})
	return etag, classifyWrite(err)
}

// PutIfMatch is compare-and-swap: succeeds iff the key's current ETag equals etag.
func (s *Store) PutIfMatch(ctx context.Context, key, etag string, body []byte) (string, Outcome) {
	var newEtag string
	err := s.op(ctx, "CAS_PUT", func(c context.Context) error {
		out, err := s.client.PutObject(c, &s3.PutObjectInput{
			Bucket:  aws.String(s.Bucket),
			Key:     aws.String(key),
			Body:    bytes.NewReader(body),
			IfMatch: aws.String(etag),
		})
		if err == nil {
			newEtag = aws.ToString(out.ETag)
		}
		return err
	})
	return newEtag, classifyWrite(err)
}

// PutIfAbsent is create-if-absent (If-None-Match: *).
func (s *Store) PutIfAbsent(ctx context.Context, key string, body []byte) (string, Outcome) {
	var etag string
	err := s.op(ctx, "CREATE_PUT", func(c context.Context) error {
		out, err := s.client.PutObject(c, &s3.PutObjectInput{
			Bucket:      aws.String(s.Bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(body),
			IfNoneMatch: aws.String("*"),
		})
		if err == nil {
			etag = aws.ToString(out.ETag)
		}
		return err
	})
	return etag, classifyWrite(err)
}

// Get reads a key. On OK, returns body and ETag; NotFound means a definite 404.
func (s *Store) Get(ctx context.Context, key string) ([]byte, string, Outcome) {
	var body []byte
	var etag string
	err := s.op(ctx, "GET", func(c context.Context) error {
		out, err := s.client.GetObject(c, &s3.GetObjectInput{
			Bucket: aws.String(s.Bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return err
		}
		defer out.Body.Close()
		body, err = io.ReadAll(out.Body)
		etag = aws.ToString(out.ETag)
		return err
	})
	return body, etag, classifyRead(err)
}

// Delete removes a key.
func (s *Store) Delete(ctx context.Context, key string) Outcome {
	err := s.op(ctx, "DELETE", func(c context.Context) error {
		_, err := s.client.DeleteObject(c, &s3.DeleteObjectInput{
			Bucket: aws.String(s.Bucket),
			Key:    aws.String(key),
		})
		return err
	})
	return classifyWrite(err)
}

// ListPage is one page of a paginated listing.
type ListPage struct {
	Keys      []string
	Truncated bool
	NextToken string
}

// List fetches one page. Pass the previous page's NextToken to continue.
// maxKeys <= 0 leaves the page size to the provider's default.
func (s *Store) List(ctx context.Context, prefix, token string, maxKeys int32) (ListPage, Outcome) {
	var page ListPage
	err := s.op(ctx, "LIST", func(c context.Context) error {
		in := &s3.ListObjectsV2Input{
			Bucket: aws.String(s.Bucket),
			Prefix: aws.String(prefix),
		}
		if maxKeys > 0 {
			in.MaxKeys = aws.Int32(maxKeys)
		}
		if token != "" {
			in.ContinuationToken = aws.String(token)
		}
		out, err := s.client.ListObjectsV2(c, in)
		if err != nil {
			return err
		}
		for _, o := range out.Contents {
			page.Keys = append(page.Keys, aws.ToString(o.Key))
		}
		page.Truncated = aws.ToBool(out.IsTruncated)
		page.NextToken = aws.ToString(out.NextContinuationToken)
		return nil
	})
	return page, classifyRead(err)
}

// DeletePrefix best-effort removes every key under prefix (test cleanup).
func (s *Store) DeletePrefix(ctx context.Context, prefix string) {
	token := ""
	for {
		page, out := s.List(ctx, prefix, token, 1000)
		if out != OK {
			return
		}
		for _, k := range page.Keys {
			s.Delete(ctx, k)
		}
		if !page.Truncated {
			return
		}
		token = page.NextToken
	}
}

func classifyWrite(err error) Outcome {
	if err == nil {
		return OK
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		code := respErr.HTTPStatusCode()
		switch {
		case code == 412:
			return PreconditionFailed
		case code == 404:
			return NotFound
		case code >= 500:
			// The server may have applied the write before failing.
			return Ambiguous
		default:
			return Failed
		}
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "PreconditionFailed" {
		return PreconditionFailed
	}
	// Transport-level failure (timeout, reset, EOF): unknown whether applied.
	return Ambiguous
}

func classifyRead(err error) Outcome {
	if err == nil {
		return OK
	}
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return NotFound
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404 {
		return NotFound
	}
	return Failed
}
