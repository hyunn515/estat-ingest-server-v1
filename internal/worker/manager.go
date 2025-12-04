// internal/worker/manager.go
package worker

import (
	"bytes"
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"estat-ingest/internal/config"
	"estat-ingest/internal/metrics"
	"estat-ingest/internal/model"
)

// Manager는 ingest 파이프라인의 중앙 조정자이다.
//
// HTTP 핸들러가 EventCh 로 넘긴 이벤트들을 모아서(batch):
//  1. JSONL + gzip 으로 인코딩하고
//  2. S3 RAW Prefix 로 업로드하며
//  3. 실패 시 로컬 DLQ 에 저장하고 재업로드를 시도한다.
//
// 주요 구성 요소:
//   - EventCh   : HTTP 수집 → Manager 로 이벤트 전달 (백프레셔의 첫 단계)
//   - collectLoop : EventCh 를 읽어 배치로 모으고, uploadCh 로 전달
//   - uploadCh  : 인코딩 + S3 업로드 작업 큐
//   - uploadLoop  : 실제 인코딩 + 업로드 + DLQ 재업로드 담당
//
// Shutdown 설계:
//   - Graceful drain 패턴을 사용한다.
//   - Shutdown() 시 EventCh 를 닫고, 내부 goroutine 이
//     남은 배치를 모두 처리한 뒤 종료될 때까지 기다린다.
//   - ctx.Done() 신호로 goroutine 을 "강제 종료"하지 않는다.
//     (강제 종료는 마지막 배치 유실(Race) 위험을 키우므로 사용하지 않음)
type Manager struct {
	cfg     config.Config
	metrics *metrics.Metrics
	s3      *S3Uploader
	dlq     *DLQManager
	encoder *Encoder

	EventCh  chan *model.Event    // HTTP 수집기가 push 하는 이벤트 큐
	uploadCh chan model.UploadJob // 인코딩/업로드 작업 큐

	ctx    context.Context
	cancel context.CancelFunc

	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewManager는 S3Uploader · DLQManager · Encoder 를 초기화하고
// 이벤트 처리 채널(EventCh, uploadCh)을 생성한다.
//
// 실제 goroutine 실행은 Start() 호출 시점에 이루어진다.
func NewManager(cfg config.Config, m *metrics.Metrics) *Manager {
	uploader := NewS3Uploader(cfg, m)
	dlq := NewDLQManager(cfg, m, uploader)
	encoder := NewEncoder()

	return &Manager{
		cfg:      cfg,
		metrics:  m,
		s3:       uploader,
		dlq:      dlq,
		encoder:  encoder,
		EventCh:  make(chan *model.Event, cfg.ChannelSize),
		uploadCh: make(chan model.UploadJob, cfg.UploadQueue),
	}
}

// Start는 ingest 파이프라인 처리용 goroutine 을 시작한다.
//
//   - collectLoop: EventCh 에서 이벤트를 받아 배치로 모으고 uploadCh 로 전달.
//   - uploadLoop : uploadCh 를 소비하면서 인코딩 + S3 업로드 + DLQ 재업로드 수행.
//
// ctx/cancel 은 S3Uploader, DLQ 처리 등의 내부 호출에서
// per-request timeout 을 묶어주는 용도로 사용되며,
// goroutine 종료 제어는 "채널 닫힘(close)"를 통해 이뤄진다.
func (m *Manager) Start() {
	m.ctx, m.cancel = context.WithCancel(context.Background())

	m.wg.Add(2)
	go m.collectLoop()
	go m.uploadLoop()
}

// Shutdown 은 graceful drain 을 수행한다.
//
// 순서:
//  1. EventCh 를 닫아서 더 이상 신규 이벤트를 받지 않는다.
//  2. collectLoop 가 남아있는 배치를 모두 flush 한 뒤 uploadCh 를 닫는다.
//  3. uploadLoop 가 uploadCh 를 모두 비우고 DLQ 재업로드까지 끝내면 종료된다.
//  4. 모든 goroutine 종료를 기다린 뒤, cancel() 로 백그라운드 자원을 정리한다.
//
// 주의:
//   - 여기서는 ctx.Cancel() 을 "먼저" 호출하지 않는다.
//     (ctx.Done() 을 select 에 사용하여 goroutine 을 끊어버리면
//     마지막 배치가 uploadCh 로 전송되기 전에 drop 될 수 있다.)
func (m *Manager) Shutdown() {
	m.stopOnce.Do(func() {
		// 더 이상 HTTP → Manager 로 이벤트가 들어오지 않도록 입구를 닫는다.
		close(m.EventCh)
	})

	// 모든 goroutine (collectLoop, uploadLoop) 종료 대기
	m.wg.Wait()

	// 마지막으로 context 취소 → 내부에서 ctx 를 참조하는 작업이 있다면 정리
	if m.cancel != nil {
		m.cancel()
	}
}

// collectLoop 는 EventCh 에서 이벤트를 읽어 배치로 묶은 뒤,
// BatchSize 또는 FlushInterval 조건이 만족되면 uploadCh 로 전달한다.
//
// Shutdown 시나리오:
//   - Shutdown() 이 EventCh 를 닫는다.
//   - 여기서는 `<-EventCh` 의 ok=false 를 감지하여
//     남아 있는 batch 를 마지막으로 flush 한 뒤 종료한다.
//   - 이때는 ctx.Done() 과 경쟁하지 않으며, 데이터를 drop 하지 않는다.
func (m *Manager) collectLoop() {
	defer m.wg.Done()
	defer close(m.uploadCh) // 더 이상 배치가 없음을 uploadLoop 에 알림

	batch := make([]*model.Event, 0, m.cfg.BatchSize)

	timer := time.NewTimer(m.cfg.FlushInterval)
	defer timer.Stop()

	// 타이머를 FlushInterval 로 재설정하는 헬퍼
	resetTimer := func() {
		if !timer.Stop() {
			// 이미 만료된 경우 채널을 비워준다.
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(m.cfg.FlushInterval)
	}

	// 일반적인 flush: uploadCh 로 block 전송.
	// - 여기서는 ctx.Done() 을 보지 않는다.
	//   (backpressure 를 그대로 전파하여 상위에서 속도 조절)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		m.uploadCh <- model.UploadJob{Events: batch} // 필요 시 여기서 block 되어 backpressure
		batch = make([]*model.Event, 0, m.cfg.BatchSize)
		resetTimer()
	}

	for {
		select {
		case ev, ok := <-m.EventCh:
			if !ok {
				// EventCh 가 닫혔다는 것은 Shutdown 시작을 의미한다.
				// 남아 있는 batch 를 마지막으로 업로드 시도 후 종료.
				flush()
				return
			}

			batch = append(batch, ev)
			if len(batch) >= m.cfg.BatchSize {
				flush()
			}

		case <-timer.C:
			// 시간 기반 flush (트래픽이 적을 때도 일정 간격으로 업로드)
			flush()
		}
	}
}

// uploadLoop 는 uploadCh 에서 배치를 꺼내어 실제 업로드를 수행한다.
//
// 작업 순서:
//  1. 배치 인코딩 (JSONL + gzip)
//  2. S3 RAW prefix 로 업로드 (실패 시 로컬 DLQ 저장)
//  3. 틈틈이 DLQ 재업로드 (ProcessOneCtx)
//
// 종료 조건:
//   - collectLoop 가 uploadCh 를 닫으면 range/recv 에서 ok=false 가 되어 종료된다.
//   - 별도로 ctx.Done() 을 select 하여 강제 종료하지 않는다.
//     (그렇게 하면 uploadCh 에 남은 배치가 처리되지 못하고 유실될 수 있음)
func (m *Manager) uploadLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case job, ok := <-m.uploadCh:
			if !ok {
				log.Println("[INFO] uploader exiting")
				return
			}

			// 이벤트 배치 처리 (인코딩 + S3 업로드 + 로컬 DLQ 저장)
			m.processUploadCtx(m.ctx, job)

			// DLQ starvation 방지: 매 배치 처리 후 최소 N건 재업로드 시도
			for i := 0; i < 3; i++ {
				m.dlq.ProcessOneCtx(m.ctx)
			}

		case <-ticker.C:
			// idle 상태에서도 주기적으로 DLQ 재처리를 진행한다.
			for i := 0; i < 3; i++ {
				m.dlq.ProcessOneCtx(m.ctx)
			}
		}
	}
}

// processUploadCtx 는 하나의 이벤트 배치에 대해
//  1. JSONL + gzip 인코딩
//  2. S3 업로드 (실패 시 로컬 DLQ 저장)
//  3. 성공/실패에 따른 metrics 업데이트
//  4. 이벤트 객체 재사용을 위한 Pool 반환
//
// 을 수행한다.
func (m *Manager) processUploadCtx(ctx context.Context, job model.UploadJob) {
	if len(job.Events) == 0 {
		return
	}

	// --- 1) JSONL + gzip 인코딩 ---
	data, err := m.encoder.EncodeBatchJSONLGZ(job.Events)
	if err != nil {
		// 인코딩 실패는 매우 드문 경우 → 원본 JSONL 을 그대로 RAW_DLQ 로 보낸다.
		// (인코딩 문제이므로 DLQManager.Save 사용 대신 직접 업로드)
		atomic.AddInt64(&m.metrics.S3PutErrorsTotal, 1)

		var buf bytes.Buffer
		for _, ev := range job.Events {
			buf.WriteString(ev.Body)
			buf.WriteByte('\n')
		}

		name := NewFilename(m.cfg.InstanceID)
		key := BuildS3Key(m.cfg.DLQPrefix, name)

		// 인코딩 실패 시 업로드도 best-effort (실패해도 추가 조치는 하지 않음)
		_ = m.s3.UploadBytesWithRetryCtx(ctx, key, buf.Bytes())
		atomic.AddInt64(&m.metrics.DLQEventsEnqueuedTotal, int64(len(job.Events)))

		// 이벤트 객체는 항상 Pool 로 반환
		m.encoder.RecycleEvents(job.Events)
		return
	}

	// --- 2) 정상 인코딩 → S3 RAW 업로드 ---
	name := NewFilename(m.cfg.InstanceID)
	key := BuildS3Key(m.cfg.RawPrefix, name)

	if err := m.s3.UploadBytesWithRetryCtx(ctx, key, data); err != nil {
		// 업로드 실패 → 로컬 DLQ 로 저장
		if err2 := m.dlq.Save(data, len(job.Events)); err2 != nil {
			log.Printf("[ERROR] local DLQ save failed: %v", err2)
		}
	} else {
		// 업로드 성공 → 저장된 이벤트 수를 metric 으로 기록
		atomic.AddInt64(&m.metrics.S3EventsStoredTotal, int64(len(job.Events)))
	}

	// --- 3) 이벤트 객체 재사용 가능하도록 Pool 반환 ---
	m.encoder.RecycleEvents(job.Events)
}
