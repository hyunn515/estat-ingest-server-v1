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
type Encoder struct{}

func NewEncoder() *Encoder {
	return &Encoder{}
}

// EncodeBatchJSONLGZ
//
// 입력 받은 이벤트 slice(배치)를 JSONL 형식으로 줄 단위 인코딩한 뒤 gzip 압축해 반환한다.
//
// [최적화 - Zero Copy Strategy]
// 기존에는 압축된 데이터를 새로운 []byte에 복사(alloc+copy)하여 반환했으나,
// 0.5GB 메모리 제한 환경에서는 이 복사 과정이 순간 메모리 피크를 2배로 만든다.
// 따라서 Pool에서 빌린 *bytes.Buffer 포인터를 그대로 반환한다.
//
// 주의:
//   - 호출자(Manager)는 반환된 버퍼 사용이 끝나면 반드시 pool.PutBuffer(buf)를 호출해야 한다.
//   - 반환된 버퍼의 소유권은 호출자에게 넘어간다.
func (e *Encoder) EncodeBatchJSONLGZ(events []*model.Event) (*bytes.Buffer, error) {

	// ------------------------------------------------------------
	// 1) gzip 결과를 담을 bytes.Buffer 를 pool에서 가져온다.
	// ------------------------------------------------------------
	buf := pool.BufferPool.Get().(*bytes.Buffer)
	buf.Reset()

	// ------------------------------------------------------------
	// 2) gzip.Writer 를 pool에서 가져오고 buffer로 reset
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
			// 실패 시 자원 정리: Gzip Writer 닫고 버퍼 반환
			_ = gz.Close()
			pool.GzipPool.Put(gz)
			pool.PutBuffer(buf) // 실패했으므로 즉시 반환(폐기)
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
	// [최적화 핵심]
	// make([]byte) + copy() 과정을 제거한다.
	// 5MB 배치를 처리할 때, 복사본을 만들면 순간 10MB가 필요하지만
	// 포인터만 넘기면 5MB로 끝난다. (OOM 방지 핵심)
	// ------------------------------------------------------------
	return buf, nil
}

// RecycleEvents 는 이벤트 slice 내 개별 Event 객체를 초기화 후
// 이벤트 풀에 반환한다.
func (e *Encoder) RecycleEvents(events []*model.Event) {
	for _, ev := range events {
		pool.ResetEvent(ev)
		pool.EventPool.Put(ev)
	}
}
