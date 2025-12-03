package server

import (
	"bytes"
	"io"
	"net/http"
	"sync/atomic"

	"estat-ingest/internal/config"
	"estat-ingest/internal/metrics"
	"estat-ingest/internal/model"
	"estat-ingest/internal/pool"
	"estat-ingest/internal/worker"
)

type Handler struct {
	cfg     config.Config
	metrics *metrics.Metrics
	worker  *worker.Manager
}

func NewHandler(cfg config.Config, m *metrics.Metrics, w *worker.Manager) *Handler {
	return &Handler{
		cfg:     cfg,
		metrics: m,
		worker:  w,
	}
}

// HandleCollect
//
// 모든 수집 요청을 처리하는 엔드포인트.
// - GET: Query String 기반
// - POST: Body 기반
//
// 공통 동작:
//  1. 요청 길이 제한(MaxBodySize)
//  2. BodyPool / EventPool 기반 메모리 재사용
//  3. ingestion queue(EventCh)에 push (full이면 drop)
//  4. metrics 증가
//
// 운영 상 의미:
//   - 이 함수는 ingest 서버의 "가장 뜨거운 경로(hot path)"로,
//     서버 성능은 이 함수의 효율성에 큰 영향을 받는다.
func (h *Handler) HandleCollect(w http.ResponseWriter, r *http.Request) {

	// 허용 메서드 검사
	if r.Method != http.MethodGet &&
		r.Method != http.MethodPost &&
		r.Method != http.MethodOptions {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// OPTIONS 요청은 CORS preflight 로 가정 → 즉시 204
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// --------------------------------------------------------------------
	// 요청 Body 최대 크기 강제 제한
	// Body가 커서 메모리가 과도하게 사용되는 것을 방지
	// --------------------------------------------------------------------
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodySize)
	defer r.Body.Close()

	var bodyStr string

	// --------------------------------------------------------------------
	// GET 방식 처리: QueryString 사용
	// --------------------------------------------------------------------
	if r.Method == http.MethodGet {

		// QueryString 크기 검사
		if len(r.URL.RawQuery) > int(h.cfg.MaxBodySize) {
			atomic.AddInt64(&h.metrics.HTTPRequestsRejectedBodyTooLargeTotal, 1)
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		bodyStr = r.URL.RawQuery

	} else {
		// ----------------------------------------------------------------
		// POST 방식 처리: BodyPool 기반 메모리 재사용
		// ----------------------------------------------------------------
		buf := pool.BodyPool.Get().(*bytes.Buffer)
		buf.Reset()
		defer pool.PutBody(buf, h.cfg.MaxBodySize*2)

		// io.Copy 는 매우 빠르고 GC-free. BodyPool 버퍼로 직접 복사.
		if _, err := io.Copy(buf, r.Body); err != nil {
			atomic.AddInt64(&h.metrics.HTTPRequestsRejectedBodyTooLargeTotal, 1)
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		bodyStr = buf.String()
	}

	// --------------------------------------------------------------------
	// Event 객체 생성 (EventPool 재사용)
	// --------------------------------------------------------------------
	ev := pool.EventPool.Get().(*model.Event)
	pool.ResetEvent(ev)

	ev.Ts = worker.Unix()        // ingest 시점 timestamp
	ev.IP = clientIP(r)          // X-Forwarded-For 기반 IP
	ev.UserAgent = r.UserAgent() // UA
	ev.Cookie = r.Header.Get("Cookie")
	ev.Body = bodyStr

	atomic.AddInt64(&h.metrics.HTTPRequestsTotal, 1)

	// --------------------------------------------------------------------
	// 이벤트를 ingestion queue(EventCh)에 push
	// Queue가 가득 찬 경우 → drop (backpressure)
	// --------------------------------------------------------------------
	select {
	case h.worker.EventCh <- ev:
		// 정상적으로 ingestion queue에 들어감
		atomic.AddInt64(&h.metrics.HTTPRequestsAcceptedTotal, 1)
		w.WriteHeader(http.StatusOK)

	default:
		// Queue Full → drop (이벤트 재사용 풀로 반환)
		pool.ResetEvent(ev)
		pool.EventPool.Put(ev)

		atomic.AddInt64(&h.metrics.HTTPRequestsRejectedQueueFullTotal, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

// HandleMetrics
//
// ingest 서버 상태를 나타내는 카운터 값들을 출력한다.
// Prometheus pull 방식으로도 쉽게 전환 가능.
func (h *Handler) HandleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, h.metrics.String())
}
