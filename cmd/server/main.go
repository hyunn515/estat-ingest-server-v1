// cmd/server/main.go
package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"estat-ingest/internal/config"
	"estat-ingest/internal/logger"
	"estat-ingest/internal/metrics"
	"estat-ingest/internal/server"
	"estat-ingest/internal/worker"

	"github.com/rs/zerolog/log"
)

func main() {

	// ====================================================================
	// CPU 설정 (Fargate vCPU 특성 대응)
	// ====================================================================
	//
	// Fargate는 vCPU 단위로 CPU share가 제한된다.
	// 예:
	//   - 0.25 vCPU = 실제 논리 CPU 1/4 만큼만 스케줄링됨
	//   - 0.5 vCPU = 실제 논리 CPU 1/2
	//
	// Go 런타임은 기본적으로 모든 CPU 코어를 GOMAXPROCS로 사용하려고 한다.
	// 하지만 Fargate에서 GOMAXPROCS를 default로 두면,
	//   - OS는 1개의 논리코어만 제공하는데
	//   - Go는 CPU 4개라고 착각하고 busy-loop scheduling → 성능 저하
	//
	// 따라서 운영에서는 GOMAXPROCS를 반드시 vCPU 수에 맞춰야 한다.
	//
	// NOTE:
	//  - 보통 Task Definition 환경변수에서 GOMAXPROCS=1 로 지정한다.
	//  - 아래 로직은 환경변수 우선 → 없으면 기본 1로 설정한다.
	// ====================================================================
	if v := os.Getenv("GOMAXPROCS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			runtime.GOMAXPROCS(n)
		}
	} else {
		runtime.GOMAXPROCS(1) // default: 1 logical CPU
	}

	// ====================================================================
	// Config & Metrics 초기화
	// ====================================================================
	//
	// - Config: 환경변수 기반으로 로드 (region, bucket, prefix, batch size 등)
	// - Metrics: /metrics 엔드포인트에서 반환하는 운영 지표 집합
	//
	// Metrics는 Prometheus 용이 아니라 운영자가 장애 원인 분석할 때
	// 중요한 내부 카운터들이다 (S3 실패 횟수, DLQ 적재, body 크기 등).
	// ====================================================================
	cfg := config.Load()
	m := metrics.New()

	// ====================================================================
	// Logger 초기화 (zerolog)
	// ====================================================================
	//
	// - Pretty 모드(LOG_PRETTY=true)에서는 개발자 친화적인 콘솔 출력
	// - 기본값: JSON 포맷 (운영환경, CloudWatch, Datadog 등에서 최적)
	// - LogLevel: debug/info/warn/error
	// - LogSampleN: info/debug 로그에 대한 샘플링 계수
	//
	// 태그(service, instance)는 모든 로그에 자동 추가된다.
	// ====================================================================
	logger.Init(cfg)
	log.Info().
		Str("service", cfg.ServiceName).
		Str("instance", cfg.InstanceID).
		Msg("logger initialized")

	// ====================================================================
	// Manager 생성 (S3Uploader + DLQManager + Encoder 포함)
	// ====================================================================
	//
	// Manager는 ingest server의 핵심 비동기 처리 엔진.
	//
	// 구성 요소:
	//  - Encoder: JSONL → gzip 변환 (고비용 CPU 작업)
	//  - S3Uploader: AWS SDK retry 0 + app-level retry
	//  - DLQManager: S3 업로드 실패 시 로컬에 저장 후 재업로드
	//  - EventCh: /collect 요청 처리 후 이벤트 전달 (백프레셔 핵심)
	//  - uploadCh: 배치 flush → 업로드 요청 전달
	//
	// 모든 비동기 goroutine은 Manager 아래에서 관리되며
	// graceful shutdown 시 안정적으로 종료된다.
	// ====================================================================
	mgr := worker.NewManager(cfg, m)
	mgr.Start()

	// ====================================================================
	// HTTP Handler 설정
	// ====================================================================
	//
	// 엔드포인트:
	//  - /collect : ingest 이벤트 수집 (핵심)
	//  - /metrics : 운영 지표 확인 (텍스트 포맷)
	//  - /health  : ALB Target Group Health check 용
	//
	// ALB가 5xx 또는 응답 지연을 감지하면 인스턴스를 교체하기 때문에
	// Health Check 응답속도는 매우 중요하다.
	// ====================================================================
	h := server.NewHandler(cfg, m, mgr)

	mux := http.NewServeMux()
	mux.HandleFunc("/collect", h.HandleCollect)
	mux.HandleFunc("/metrics", h.HandleMetrics)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	// ====================================================================
	// HTTP 서버 설정 (Timeout 매우 중요)
	// ====================================================================
	//
	// - Read/WriteTimeout은 비정상 커넥션이 리소스를 점유하는 것을 방지한다.
	// - IdleTimeout은 ALB Idle Timeout보다 "조금 길게" 설정해야 한다.
	//
	// 예:
	//   ALB Idle Timeout = 60초 → 서버 IdleTimeout = 65초
	//
	// 둘이 같으면, 두 endpoint가 동시에 연결을 끊으려는 race로
	// 502/504 문제가 발생한다.
	// ====================================================================
	srv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      mux,
		ReadTimeout:  8 * time.Second,
		WriteTimeout: 8 * time.Second,
		IdleTimeout:  65 * time.Second,
	}

	// ====================================================================
	// Graceful Shutdown (ECS/Fargate scale-in)
	// ====================================================================
	//
	// ECS는 scale-in 또는 rolling deploy 시:
	//   1) SIGTERM 전송 → 30초 Grace Period
	//   2) 이후 SIGKILL 강제 종료
	//
	// 우리는 SIGTERM 수신 시:
	//   - HTTP 서버 먼저 멈춰서 더 이상 요청 수신하지 않음
	//   - Manager.Shutdown() 호출하여 내부 goroutine 안전 종료
	//
	// Shutdown 순서는 매우 중요:
	//   - EventCh → UploadCh 순서로 닫아야 panic 방지
	//   - context cancel() 은 가장 마지막에 수행해야 flush 중단 방지
	// ====================================================================
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

		sig := <-sigCh
		log.Warn().
			Str("signal", sig.String()).
			Msg("shutdown signal received")

		// 1) HTTP 서버 종료 (새 요청 막기)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := srv.Shutdown(ctx); err != nil {
			log.Error().Err(err).Msg("http shutdown failed")
		}
		cancel()

		// 2) Manager 종료 (flush + DLQ 재업로드 포함)
		log.Info().Msg("stopping worker manager...")
		mgr.Shutdown()
	}()

	// ====================================================================
	// 서버 시작
	// ====================================================================
	log.Info().
		Str("addr", cfg.HTTPAddr).
		Msg("ingest server listening")

	// 블로킹
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("http server terminated unexpectedly")
	}

	// Manager는 idempotent 종료
	mgr.Shutdown()
	log.Info().Msg("shutdown complete")
}
