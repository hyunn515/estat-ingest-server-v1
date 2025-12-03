package worker

import (
	"bytes"

	"estat-ingest/internal/model"
	"estat-ingest/internal/pool"

	json "github.com/goccy/go-json"
	"github.com/klauspost/compress/gzip"
)

// Encoder 는 이벤트 배치를 JSONL → gzip 형태로 직렬화하는 컴포넌트.
// 전체 ingest 파이프라인에서 CPU 사용량과 메모리 사용량에
// 가장 큰 영향을 주는 핵심 구간이다.
//
// 특징:
//   - 고성능 goccy/json 기반 JSON 인코딩
//   - gzip.Writer + bytes.Buffer 재사용(pool 기반)
//   - 결과는 최종적으로 새로운 []byte 로 복사해 호출자에게 소유권을 넘김
//     (pool 버퍼를 그대로 반환하면 데이터 corruption 위험)
type Encoder struct{}

func NewEncoder() *Encoder {
	return &Encoder{}
}

// EncodeBatchJSONLGZ 는 입력 받은 이벤트 slice(배치)를
// JSONL 형식으로 줄 단위 인코딩한 뒤 gzip 압축해 반환한다.
//
// 이벤트 수(batch size)가 커질수록 CPU 부하가 매우 높아지므로
// 운영 배치 크기는 반드시 성능 테스트 기반으로 설정해야 한다.
//
// 반환값:
// - data: 압축된 결과의 byte slice(호출자 소유)
// - err: 인코딩 과정 중 오류 발생 시
func (e *Encoder) EncodeBatchJSONLGZ(events []*model.Event) ([]byte, error) {

	// ------------------------------------------------------------
	// 1) gzip 결과를 담을 bytes.Buffer 를 pool에서 가져온다.
	//    (기본 cap=256KB, 1MB 초과 시 pool에 반환하지 않음 — pool.go 참고)
	// ------------------------------------------------------------
	buf := pool.BufferPool.Get().(*bytes.Buffer)
	buf.Reset()

	// ------------------------------------------------------------
	// 2) gzip.Writer 를 pool에서 가져오고 buffer로 reset
	//    (gzip.BestSpeed 사용 중)
	// ------------------------------------------------------------
	gz := pool.GzipPool.Get().(*gzip.Writer)
	gz.Reset(buf)

	// ------------------------------------------------------------
	// 3) goccy/go-json encoder 생성 (gzip writer에 직결)
	// ------------------------------------------------------------
	enc := json.NewEncoder(gz)

	// ------------------------------------------------------------
	// 4) JSONL 인코딩
	//    이벤트마다 한 줄씩 JSON 인코딩 → gz writer로 바로 write
	// ------------------------------------------------------------
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			// gzip close 먼저 시도
			_ = gz.Close()
			pool.GzipPool.Put(gz)
			pool.PutBuffer(buf)
			return nil, err
		}
	}

	// ------------------------------------------------------------
	// 5) gzip footer flush & close
	//    Close() 시 압축 스트림이 완성됨.
	// ------------------------------------------------------------
	if err := gz.Close(); err != nil {
		pool.GzipPool.Put(gz)
		pool.PutBuffer(buf)
		return nil, err
	}
	pool.GzipPool.Put(gz)

	// ------------------------------------------------------------
	// 6) buf.Bytes() 를 caller가 소유하는 새로운 slice로 복사
	//    (pool 버퍼는 재사용되므로 그대로 반환하면 안 됨)
	// ------------------------------------------------------------
	raw := buf.Bytes()
	data := make([]byte, len(raw))
	copy(data, raw)

	// ------------------------------------------------------------
	// 7) buffer pool로 반환
	// ------------------------------------------------------------
	pool.PutBuffer(buf)

	return data, nil
}

// RecycleEvents 는 이벤트 slice 내 개별 Event 객체를 초기화 후
// 이벤트 풀에 반환한다.
// 모델 구조체가 큰 경우 이 재사용으로 GC pressure 감소 효과가 크다.
func (e *Encoder) RecycleEvents(events []*model.Event) {
	for _, ev := range events {
		pool.ResetEvent(ev)
		pool.EventPool.Put(ev)
	}
}
