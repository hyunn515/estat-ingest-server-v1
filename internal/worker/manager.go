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

// Manager는 ingest 시스템의 핵심 파이프라인이다.
// HTTP 요청으로 들어온 이벤트(EventCh)를 모아서(batch)
//   - gzip+JSONL로 인코딩
//   - S3 업로드 (실패 시 DLQ 저장)
//
// 하는 전체 흐름을 제어한다.
//
// 주요 구성:
//   - EventCh: HTTP → Manager로 이벤트 전달
//   - collectLoop: 이벤트를 배치 사이즈 또는 FlushInterval마다 묶어서 uploadCh에 전달
//   - uploadCh: 인코딩 및 S3 업로드 작업 큐
//   - uploadLoop: 실제 업로드 및 DLQ 처리 담당
//
// Manager는 graceful shutdown을 지원하며,
// 모든 배치 처리가 끝나야 종료된다.
type Manager struct {
	cfg     config.Config
	metrics *metrics.Metrics
	s3      *S3Uploader
	dlq     *DLQManager
	encoder *Encoder

	EventCh  chan *model.Event    // HTTP 수집기가 push
	uploadCh chan model.UploadJob // 인코딩/업로드 작업 큐

	ctx    context.Context
	cancel context.CancelFunc

	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewManager는 S3Uploader · DLQManager · Encoder를 초기화하고
// 이벤트 처리 채널을 구성한다.
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

// Start는 두 개의 주요 goroutine을 실행한다.
//   - collectLoop: 이벤트를 batch로 모아서 uploadCh로 전달
//   - uploadLoop: batch 인코딩 + S3 업로드 + DLQ 처리
func (m *Manager) Start() {
	m.ctx, m.cancel = context.WithCancel(context.Background())

	m.wg.Add(2)
	go m.collectLoop()
	go m.uploadLoop()
}

// Shutdown은 graceful shutdown을 위해
// EventCh를 먼저 닫고 모든 goroutine이 완료될 때까지 대기한다.
func (m *Manager) Shutdown() {
	m.stopOnce.Do(func() {
		m.cancel()
		close(m.EventCh)
	})
	m.wg.Wait()
}

// collectLoop는 EventCh에서 이벤트를 읽어 batch로 묶는다.
// BatchSize 도달 또는 FlushInterval 타이머 만료 시 uploadCh에 전달한다.
//
// flush()는 항상 새로운 batch slice를 생성하여
// 재사용으로 인한 데이터 오염을 방지한다.
func (m *Manager) collectLoop() {
	defer m.wg.Done()
	defer close(m.uploadCh)

	batch := make([]*model.Event, 0, m.cfg.BatchSize)
	timer := time.NewTimer(m.cfg.FlushInterval)
	defer timer.Stop()

	reset := func() {
		// 타이머가 이미 만료된 상태면 drain
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(m.cfg.FlushInterval)
	}

	flush := func() {
		if len(batch) > 0 {
			select {
			case m.uploadCh <- model.UploadJob{Events: batch}:
			case <-m.ctx.Done():
				return
			}
			// 새로운 slice로 교체(기존 slice 재사용 금지)
			batch = make([]*model.Event, 0, m.cfg.BatchSize)
			reset()
		}
	}

	for {
		select {
		case <-m.ctx.Done():
			// 종료 시 남아있는 batch도 업로드 시도
			flush()
			return

		case ev, ok := <-m.EventCh:
			if !ok {
				// 이벤트 채널 종료 → 남은 batch 처리 후 종료
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= m.cfg.BatchSize {
				flush()
			}

		case <-timer.C:
			// FlushInterval 도달 → batch 업로드
			flush()
		}
	}
}

// uploadLoop는 uploadCh에서 batch를 받아
//  1. gzip+JSONL 인코딩
//  2. S3 업로드 (실패 시 DLQ 저장)
//  3. DLQ 재업로드 3회 (starvation 방지)
//
// 를 수행한다.
//
// uploadCh가 닫히면 모든 작업을 마치고 종료된다.
func (m *Manager) uploadLoop() {
	defer m.wg.Done()

	for {
		select {
		case <-m.ctx.Done():
			return

		case job, ok := <-m.uploadCh:
			if !ok {
				log.Println("[INFO] uploader exiting")
				return
			}

			// 이벤트 배치 처리
			m.processUploadCtx(m.ctx, job)

			// DLQ starvation 방지 — 최소 3건씩 처리
			for i := 0; i < 3; i++ {
				m.dlq.ProcessOneCtx(m.ctx)
			}

		default:
			// idle 시에도 DLQ 재업로드 진행
			for i := 0; i < 3; i++ {
				m.dlq.ProcessOneCtx(m.ctx)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// processUploadCtx는 하나의 이벤트 배치를 처리한다.
//  1. JSONL + gzip 인코딩 실패 → raw_dlq 업로드
//  2. S3 업로드 실패 → 로컬 DLQ 저장
//  3. 성공 시 metrics 업데이트
//  4. 이벤트 객체는 풀에 반환(Recycle)
func (m *Manager) processUploadCtx(ctx context.Context, job model.UploadJob) {
	if len(job.Events) == 0 {
		return
	}

	// --- 1) JSONL + gzip 인코딩 ---
	data, err := m.encoder.EncodeBatchJSONLGZ(job.Events)
	if err != nil {
		// 인코딩 실패는 매우 드문 경우 → 원본 JSONL을 S3 raw_dlq로 업로드
		atomic.AddInt64(&m.metrics.S3PutErrorsTotal, 1)

		var buf bytes.Buffer
		for _, ev := range job.Events {
			buf.WriteString(ev.Body)
			buf.WriteByte('\n')
		}

		name := NewFilename(m.cfg.InstanceID)
		key := BuildS3Key(m.cfg.DLQPrefix, name)

		_ = m.s3.UploadBytesWithRetryCtx(ctx, key, buf.Bytes()) // 실패해도 best-effort
		atomic.AddInt64(&m.metrics.DLQEventsEnqueuedTotal, int64(len(job.Events)))

		// 인코딩 실패 시에도 이벤트 객체는 항상 반환
		m.encoder.RecycleEvents(job.Events)
		return
	}

	// --- 2) 정상 인코딩 → S3 raw 업로드 ---
	name := NewFilename(m.cfg.InstanceID)
	key := BuildS3Key(m.cfg.RawPrefix, name)

	if err := m.s3.UploadBytesWithRetryCtx(ctx, key, data); err != nil {
		// 업로드 실패 → 로컬 DLQ로 저장
		if err2 := m.dlq.Save(data, len(job.Events)); err2 != nil {
			log.Printf("[ERROR] local DLQ save failed: %v", err2)
		}
	} else {
		// 업로드 성공
		atomic.AddInt64(&m.metrics.S3EventsStoredTotal, int64(len(job.Events)))
	}

	// --- 3) 이벤트 객체 재사용 가능하도록 반환 ---
	m.encoder.RecycleEvents(job.Events)
}
