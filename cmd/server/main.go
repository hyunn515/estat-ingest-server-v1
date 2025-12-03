package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"estat-ingest/internal/config"
	"estat-ingest/internal/metrics"
	"estat-ingest/internal/server"
	"estat-ingest/internal/worker"
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
	//   - Go는 CPU 4개라고 착각하고 busy-loop scheduling 발생 → 성능 저하
	//
	// 따라서 운영에서는 GOMAXPROCS를 반드시 vCPU 수에 맞춰야 함.
	//
	// Fargate Task Definition 환경변수에서 GOMAXPROCS=1 로 지정하는 것을 권장.
	//
	// 재정의 가능: 운영팀이 Task마다 다르게 조정할 수 있음.
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
	// 중요한 내부 카운터들이다 (S3 실패횟수, DLQ 적재, 바디크기 등).
	// ====================================================================
	cfg := config.Load()
	m := metrics.New()

	// ====================================================================
	// Manager 생성 (S3Uploader + DLQManager + Encoder 포함)
	// ====================================================================
	//
	// Manager는 ingest server의 핵심 비동기 처리 엔진.
	//
	// 구성 요소:
	//  - Encoder: JSONL → gzip 압축 변환 (고비용 CPU 작업)
	//  - S3Uploader: AWS SDK retry + timeout 적용된 업로드
	//  - DLQManager: S3 업로드 실패 시 로컬에 저장 후 재업로드
	//  - EventCh: /collect 요청 처리 후 이벤트 전달 (백프레셔 핵심)
	//  - uploadCh: 배치가 flush된 후 업로드 요청 전달
	//
	// 모든 비동기 goroutine은 Manager 아래에서 관리됨
	// ECS/Fargate가 SIGTERM 보낼 때 graceful 종료가 가능해야 한다.
	// ====================================================================
	mgr := worker.NewManager(cfg, m)
	mgr.Start()

	// ====================================================================
	// HTTP Handler 설정
	// ====================================================================
	//
	// 엔드포인트:
	//  - /collect : ingest 이벤트 수집 (핵심)
	//  - /metrics : 운영 지표 확인
	//  - /health  : ALB Target Group Health check용
	//
	// ALB가 5xx 또는 응답지연을 감지하면 인스턴스를 교체하기 때문에
	// Health Check 응답속도는 매우 중요하다.
	// ====================================================================
	h := server.NewHandler(cfg, m, mgr)

	mux := http.NewServeMux()
	mux.HandleFunc("/collect", h.HandleCollect)
	mux.HandleFunc("/metrics", h.HandleMetrics)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		// ALB는 단순 문자열로도 health 판단 가능
		w.Write([]byte("ok"))
	})

	// ====================================================================
	// HTTP 서버 설정 (Timeout 매우 중요)
	// ====================================================================
	//
	// ReadTimeout / WriteTimeout:
	//  - FE에서 오는 요청은 매우 짧은 JSON payload
	//  - Timeout을 짧게 잡아야 비정상 커넥션이 서버 리소스 점유하는 것을 방지
	//
	// IdleTimeout:
	//  - ALB → ECS 연결에서 keep-alive 연결 관리 목적
	// ====================================================================
	srv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      mux,
		ReadTimeout:  8 * time.Second,
		WriteTimeout: 8 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ====================================================================
	// Graceful Shutdown (ECS/Fargate scale-in 대응)
	// ====================================================================
	//
	// ECS는 scale-in 또는 deploy rolling update 시
	//   1) SIGTERM 전송 → 30초 Grace Period
	//   2) 이후 SIGKILL
	//
	// 우리는 SIGTERM 수신 시:
	//   - HTTP 서버 먼저 멈추고 (더 이상 요청 받지 않음)
	//   - 내부 Manager 취소 (goroutine 안전 종료)
	//
	// 이를 통해 업로드 중인 S3 요청이나 DLQ 작업 도중 중단되어
	// 데이터 유실되는 것을 방지한다.
	// ====================================================================
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

		sig := <-sigCh
		log.Printf("[INFO] shutdown signal received: %v", sig)

		// 1) HTTP 서버 종료
		//    ALB는 이 시점부터 트래픽을 새 인스턴스로 라우팅한다.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("[ERROR] http shutdown: %v", err)
		}
		cancel()

		// 2) 내부 worker 종료 (DLQ flush 포함)
		log.Println("[INFO] stopping worker manager...")
		mgr.Shutdown()
	}()

	// ====================================================================
	// 서버 시작
	// ====================================================================
	log.Printf("[INFO] ingest server listening on %s", cfg.HTTPAddr)

	// ListenAndServe는 blocking 함수 → 종료되면 에러 확인
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[FATAL] http server terminated: %v", err)
	}

	// Manager가 이미 종료되어 있더라도 다시 호출해도 safe
	mgr.Shutdown()
	log.Println("[INFO] shutdown complete")
}
