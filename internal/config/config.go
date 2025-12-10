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

	ServiceName string // ingest 서비스 이름 (로깅용 태그)
	InstanceID  string // ingest 프로세스 고유 ID (호스트명 기반, 실패 시 랜덤 hex)
	HTTPAddr    string // HTTP 서버 bind 주소 (예: ":8080")

	// ---------------------------
	// 로깅 설정
	// ---------------------------
	// 로그 설정은 "서비스 동작"에는 영향을 주지 않지만,
	// 운영 시 로그 볼륨 조절과 개발 환경에서의 가독성을 위해 제공된다.
	// 값이 비어있거나 잘못되어도 기본값으로 대체되며, 프로세스를 죽이지 않는다.
	// --------------------------------------------
	// LogLevel:
	//   - 최소 출력 레벨. 이 레벨보다 낮은 로그는 버려진다.
	//   - 예: "debug" | "info" | "warn" | "error"
	//   - 비어있으면 "info" 로 동작한다.
	//
	// LogPretty:
	//   - true  → 개발용 텍스트(콘솔) 포맷
	//   - false → JSON 포맷 (운영/수집 시스템에서 사용하기 적합)
	//
	// LogSampleN:
	//   - Info/Debug 로그에 대한 샘플링 계수.
	//   - 1 이면 샘플링 없음(모두 출력), N 이면 대략 1/N 로그만 출력.
	//   - Error/Warn 레벨은 샘플링 대상이 아니다(항상 출력).
	// --------------------------------------------

	LogLevel   string // 최소 로그 레벨 문자열 (비어있으면 "info")
	LogPretty  bool   // 사람이 읽기 쉬운 pretty logging 사용 여부
	LogSampleN int    // Info/Debug 로그 샘플링 계수 (1=샘플링 없음)

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
	// Retry 정책 단일화
	// --------------------------------------------
	// AWS SDK v2 기본 retry는 서비스 상황에 따라 3회까지 수행되며,
	// 코드 레벨 retry 와 겹치면 예측 불가능한 처리 지연이 발생한다.
	//
	// → 운영 안정성을 위해 SDK Retry는 코드에서 0으로 고정한다.
	// → "재시도 횟수"는 오직 애플리케이션 레벨(S3AppRetries)만 사용한다.
	// --------------------------------------------

	S3Timeout    time.Duration // 각 S3 PutObject 시도당 timeout
	S3AppRetries int           // S3 업로드 재시도 횟수 (SDK retry는 항상 0)

	// ---------------------------
	// 로컬 DLQ (Dead Letter Queue)
	// ---------------------------

	DLQDir          string        // 로컬 DLQ 디렉토리 경로
	DLQMaxAge       time.Duration // DLQ 파일 TTL (초과 시 삭제)
	DLQMaxSizeBytes int64         // DLQ 전체 허용 용량 (바이트)
}

// Load
//
// 환경 변수 기반으로 Config 값을 초기화한다.
// 필수 env 가 비어있으면 즉시 프로세스를 종료(fail-fast).
// 운영/배포 환경에서 반드시 설정해야 하는 값들이다.
func Load() Config {
	return Config{
		AWSRegion: must("AWS_REGION"),

		RawBucket: must("RAW_BUCKET"),
		RawPrefix: must("RAW_PREFIX"),
		DLQPrefix: must("DLQ_PREFIX"),

		ServiceName: "estat-ingest",
		InstanceID:  fallbackInstanceID(),
		HTTPAddr:    must("HTTP_ADDR"),

		LogLevel:   getenvDefault("LOG_LEVEL", "info"),
		LogPretty:  optBool("LOG_PRETTY", false),
		LogSampleN: optInt("LOG_SAMPLE_N", 1),

		MaxBodySize:   mustInt64("MAX_BODY_SIZE"),
		ChannelSize:   mustInt("CHANNEL_SIZE"),
		UploadQueue:   mustInt("UPLOAD_QUEUE"),
		BatchSize:     mustInt("BATCH_SIZE"),
		FlushInterval: mustDur("FLUSH_INTERVAL"),

		S3Timeout:    mustDur("S3_TIMEOUT"),
		S3AppRetries: mustInt("S3_APP_RETRIES"),

		DLQDir:          must("DLQ_DIR"),
		DLQMaxAge:       mustDur("DLQ_MAX_AGE"),
		DLQMaxSizeBytes: mustInt64("DLQ_MAX_SIZE_BYTES"),
	}
}

// must / mustInt / mustInt64 / mustDur
//
// 공통 패턴.
// 필수 환경변수가 없거나 형식이 잘못되면 즉시 로그 출력 후 종료(fail-fast).
// 런타임 중 설정 오류를 겪지 않도록 하기 위한 보호 전략.
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

// 선택적(optional) 환경변수 유틸리티
//
// - 필수값이 아니기 때문에, 비어 있거나 잘못되더라도 프로세스를 종료하지 않는다.
// - 잘못된 값은 로그 한 줄 남기고 기본값으로 대체한다.
//
// 로그 관련 설정(LOG_LEVEL, LOG_PRETTY, LOG_SAMPLE_N)은
// 여기 함수들을 통해 초기화된다.

func getenvDefault(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func optBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	// strconv.ParseBool 은 "1", "t", "T", "TRUE", "true", "True" 등을 true 로 인식.
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("invalid bool env %s=%q: %v (fallback=%v)", key, v, err, def)
		return def
	}
	return b
}

func optInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid int env %s=%q: %v (fallback=%d)", key, v, err, def)
		return def
	}
	if n <= 0 {
		// 샘플링 계수 등에서 0 이하는 의미가 없으므로 기본값으로 되돌린다.
		log.Printf("non-positive int env %s=%q: fallback=%d", key, v, def)
		return def
	}
	return n
}

// fallbackInstanceID
//
// 이 ingest 서버 인스턴스를 식별하는 고유 값.
//   - 기본: hostname (ECS/Fargate에서는 task-id 형태로 고유)
//   - fallback: 12자리 랜덤 hex
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
