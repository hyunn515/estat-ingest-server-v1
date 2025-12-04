// internal/worker/s3_uploader.go
package worker

import (
	"bytes"
	"context"
	"io"
	"log"
	"sync/atomic"
	"time"

	"estat-ingest/internal/config"
	"estat-ingest/internal/metrics"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsCfgLib "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Uploader는 S3 업로드 기능을 담당하는 구성 요소이다.
// - JSONL.gz 바이트 업로드 (UploadBytesWithRetryCtx)
// - 로컬 DLQ 파일 업로드 (UploadFileWithRetryCtx)
// - 내부적으로 AWS SDK v2 client 사용
//
// 모든 업로드는 컨텍스트 기반(timeout + cancel-safe)으로 이루어지며,
// 재시도(backoff) 로직을 포함한다.
type S3Uploader struct {
	cfg     config.Config
	metrics *metrics.Metrics
	client  *s3.Client
}

// NewS3Uploader는 AWS SDK Config를 초기화하고 S3 client를 생성한다.
func NewS3Uploader(cfg config.Config, m *metrics.Metrics) *S3Uploader {
	return &S3Uploader{
		cfg:     cfg,
		metrics: m,
		client:  newS3Client(cfg),
	}
}

// newS3Client는 AWS 지역(region)과 Retry 설정 등 기본 옵션을 로드한다.
// 실패 시 fatal 로그 후 즉시 종료한다 (운영 환경에서는 필수).
func newS3Client(cfg config.Config) *s3.Client {
	awsCfg, err := awsCfgLib.LoadDefaultConfig(
		context.TODO(),
		awsCfgLib.WithRegion(cfg.AWSRegion),
	)
	if err != nil {
		log.Fatalf("[FATAL] failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.RetryMaxAttempts = 0
	})

	return client
}

// UploadBytesWithRetryCtx
// -----------------------
// 메모리에 이미 존재하는 gzip+JSONL 바이트 배열을 S3로 업로드한다.
// - 각 업로드는 5초 timeout
// - retry + exponential backoff 포함
// - shutdown-safe: ctx.Done() 시 즉시 중단
//
// body는 매 재시도마다 reader를 새로 만들어야 하므로 bytes.NewReader 사용.
func (u *S3Uploader) UploadBytesWithRetryCtx(
	ctx context.Context,
	key string,
	body []byte,
) error {

	var lastErr error
	backoff := 200 * time.Millisecond

	for attempt := 1; attempt <= u.cfg.S3AppRetries; attempt++ {

		// shutdown 체크
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		reader := bytes.NewReader(body)

		// 실제 S3 업로드
		if err := u.putObject(ctx, key, reader, int64(len(body))); err == nil {
			return nil
		} else {
			lastErr = err
			atomic.AddInt64(&u.metrics.S3PutErrorsTotal, 1)
		}

		// backoff 적용 (최대 2초)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}
	}

	return lastErr
}

// UploadFileWithRetryCtx
// -----------------------
// 로컬 DLQ에 저장된 파일을 그대로 S3로 업로드할 때 사용한다.
// - io.ReadSeeker를 사용하여 retry 시 Seek(0)으로 rewind 가능
// - shutdown-safe + retry/backoff 동일 적용
// - 파일 크기는 caller에서 받아 전달한다.
func (u *S3Uploader) UploadFileWithRetryCtx(
	ctx context.Context,
	key string,
	f io.ReadSeeker,
	size int64,
) error {

	var lastErr error
	backoff := 200 * time.Millisecond

	for attempt := 1; attempt <= u.cfg.S3AppRetries; attempt++ {

		// shutdown 체크
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// 실제 업로드 호출
		if err := u.putObject(ctx, key, f, size); err == nil {
			return nil
		} else {
			lastErr = err
			atomic.AddInt64(&u.metrics.S3PutErrorsTotal, 1)
		}

		// backoff 적용
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}

		// retry 시 파일 포인터를 처음으로 되돌린다 (반드시 필요)
		f.Seek(0, io.SeekStart)
	}

	return lastErr
}

// putObject
// ---------
// 실제 AWS S3 PutObject 호출을 수행한다.
// - retries는 caller에서 제어하며 여기서는 1회 호출만 담당
// - 각 호출은 컨텍스트 기반 5초 timeout을 가진다
//
// bucket은 RawBucket 또는 DLQPrefix에 따라 달라지며,
// key는 caller가 완성하여 전달한다.
func (u *S3Uploader) putObject(
	ctx context.Context,
	key string,
	body io.Reader,
	size int64,
) error {

	// 1회 시도당 timeout 적용
	ctx2, cancel := context.WithTimeout(ctx, u.cfg.S3Timeout)
	defer cancel()

	_, err := u.client.PutObject(ctx2, &s3.PutObjectInput{
		Bucket:        aws.String(u.cfg.RawBucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
	})

	return err
}
