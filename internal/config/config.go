// internal/config/config.go
package config

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"strconv"
	"time"
)

// Config
//
// 서비스 실행 시 필요한 모든 환경 변수 값을 보관하는 구조체.
// 모든 값은 프로세스 시작 시점에 Load() 에 의해 초기화되며,
// 이후에는 변경되지 않는 불변(read-only) 설정들이다.
//
// 운영/개발 환경 인프라 팀이 설정해야 할 값들이므로,
// 필드 하나하나가 의미하는 바를 주석으로 명확히 남겨둔다.
type Config struct {

	// ---------------------------
	// AWS / S3 기본 환경
	// ---------------------------

	AWSRegion string // AWS 리전 (예: ap-northeast-2)

	RawBucket string // 수집 데이터가 저장될 S3 버킷 이름
	RawPrefix string // RAW 데이터 저장 경로 prefix (예: raw/)
	DLQPrefix string // DLQ 데이터 저장 경로 prefix (예: dlq/)

	// ---------------------------
	// 서버 식별자 / 네트워크
	// ---------------------------

	InstanceID string // 이 ingest 프로세스 고유 ID (호스트명 기반, 실패 시 랜덤)
	HTTPAddr   string // HTTP 서버 bind 주소 (예: ":8080")

	// ---------------------------
	// 요청 처리 파라미터
	// ---------------------------

	MaxBodySize   int64         // 단일 HTTP 요청 body 최대 크기 (바이트)
	ChannelSize   int           // EventCh 버퍼 크기
	UploadQueue   int           // uploadCh 버퍼 크기
	BatchSize     int           // 배치 크기 (N개 모이면 S3로 업로드)
	FlushInterval time.Duration // 배치 flush 주기 (시간 기반 flush)

	// ---------------------------
	// S3 업로드 설정
	// ---------------------------

	S3Timeout    time.Duration // 각 S3 PutObject attempt의 timeout
	S3MaxRetries int           // S3 업로드 재시도 횟수

	// ---------------------------
	// 로컬 DLQ (Dead Letter Queue)
	// ---------------------------

	DLQDir          string        // 로컬에 저장할 DLQ 디렉토리 경로
	DLQMaxAge       time.Duration // DLQ 파일 TTL (지나면 삭제)
	DLQMaxSizeBytes int64         // DLQ 디렉토리 전체 허용 용량 (바이트)
}

// Load
//
// 환경 변수 기반으로 Config 값을 초기화한다.
// 필수 env 가 비어있으면 즉시 프로세스를 종료(fail-fast).
// 환경 변수는 인프라/배포 환경에서 반드시 설정해야 하는 값들이다.
func Load() Config {
	return Config{
		AWSRegion: must("AWS_REGION"),

		RawBucket: must("RAW_BUCKET"),
		RawPrefix: must("RAW_PREFIX"),
		DLQPrefix: must("DLQ_PREFIX"),

		InstanceID: fallbackInstanceID(),
		HTTPAddr:   must("HTTP_ADDR"),

		MaxBodySize:   mustInt64("MAX_BODY_SIZE"),
		ChannelSize:   mustInt("CHANNEL_SIZE"),
		UploadQueue:   mustInt("UPLOAD_QUEUE"),
		BatchSize:     mustInt("BATCH_SIZE"),
		FlushInterval: mustDur("FLUSH_INTERVAL"),

		S3Timeout:    mustDur("S3_TIMEOUT"),
		S3MaxRetries: mustInt("S3_MAX_RETRIES"),

		DLQDir:          must("DLQ_DIR"),
		DLQMaxAge:       mustDur("DLQ_MAX_AGE"),
		DLQMaxSizeBytes: mustInt64("DLQ_MAX_SIZE_BYTES"),
	}
}

// must / mustInt / mustInt64 / mustDur
//
// 공통 패턴.
// 필수 환경변수가 없거나, 형식이 잘못되면 즉시 로그 출력 후 종료한다.
// 런타임 중 설정 오류를 겪지 않도록 하기 위한 fail-fast 전략.
func must(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env: %s", key)
	}
	return v
}

func mustInt(key string) int {
	v := must(key)
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("invalid int env %s=%q: %v", key, v, err)
	}
	return n
}

func mustInt64(key string) int64 {
	v := must(key)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Fatalf("invalid int64 env %s=%q: %v", key, v, err)
	}
	return n
}

func mustDur(key string) time.Duration {
	v := must(key)
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("invalid duration env %s=%q: %v", key, v, err)
	}
	return d
}

// fallbackInstanceID
//
// 이 인스턴스를 식별하는 고유 값.
//   - 가능한 경우 hostname 사용
//   - Kubernetes/ECS/Fargate 환경에서는 hostname 자체가 고유 ID 역할을 함
//   - 실패하거나 공백이면 12자리 랜덤 hex 값 생성
func fallbackInstanceID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	// 랜덤 6바이트 → 12자리 hex
	var b [6]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}
