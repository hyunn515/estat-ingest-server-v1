package pool

import (
	"bytes"
	"sync"

	"estat-ingest/internal/model"

	"github.com/klauspost/compress/gzip"
)

// ---------------------------------------------------------------
// Pool 구성 목적
//
// ingest 서버는 초당 수천~수만 요청을 처리하며,
// 매 요청마다 body 읽기, event 객체 생성, gzip 결과 버퍼 생성 등
// 메모리 할당이 매우 빈번하게 발생한다.
//
// 아래 Pool들은 "GC 줄이기, 메모리 재사용, 성능 안정화" 목적.
// ---------------------------------------------------------------

var (
	// EventPool:
	//   - Event 객체 재사용
	//   - 매 요청마다 new(model.Event) 하지 않도록 함
	EventPool = sync.Pool{
		New: func() any { return new(model.Event) },
	}

	// BodyPool:
	//   - POST body를 임시 저장하는 버퍼
	//   - 초기 용량 4KB (대부분의 small POST는 여기에 수용됨)
	//   - 너무 큰 버퍼는 caller(maxCap 조건)에서 재사용하지 않음
	BodyPool = sync.Pool{
		New: func() any {
			return bytes.NewBuffer(make([]byte, 0, 4*1024))
		},
	}

	// BufferPool:
	//   - gzip 인코딩 결과를 담는 임시 버퍼
	//   - 초기 용량 256KB (일반적인 배치 사이즈에 최적화)
	//   - 1MB 초과 버퍼는 메모리 폭주 방지를 위해 풀에 넣지 않음
	BufferPool = sync.Pool{
		New: func() any {
			return bytes.NewBuffer(make([]byte, 0, 256*1024))
		},
	}

	// GzipPool:
	//   - gzip.Writer 재사용 (매번 new 하면 비용 매우 큼)
	//   - BestSpeed 옵션: ingest 서버 특성상 속도 우선 전략
	GzipPool = sync.Pool{
		New: func() any {
			w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
			return w
		},
	}
)

// Pool에 되돌려줄 최대 gzip 버퍼 용량
// 이보다 큰 버퍼는 Pool에 넣지 않고 GC에게 위임해
// 메모리 폭발을 예방.
const MaxBufferCap = 1 * 1024 * 1024 // 1MB

// ResetEvent:
//   - Event 구조체를 재사용할 수 있도록 zeroing.
//   - 단일 구조체 리셋 비용은 작으며 메모리 단편화 없이 안정적.
func ResetEvent(e *model.Event) {
	*e = model.Event{}
}

// PutBody:
//   - BodyPool에 buf를 반환할지 결정.
//   - maxCap(보통 MaxBodySize*2)보다 크면 버려서 GC로.
//   - 너무 큰 POST body가 들어왔을 때 메모리를 계속 보유하지 않도록 설계.
func PutBody(buf *bytes.Buffer, maxCap int64) {
	if int64(buf.Cap()) <= maxCap {
		buf.Reset()
		BodyPool.Put(buf)
	}
	// 그 외는 반환하지 않고 자연스럽게 GC 처리
}

// PutBuffer:
//   - gzip 결과 버퍼 반환
//   - 1MB 이하이면 풀에 재사용
//   - 초대형 배치 gzip 결과는 풀로 돌리지 않음 → 메모리 안정화 목적
func PutBuffer(buf *bytes.Buffer) {
	if buf.Cap() <= MaxBufferCap {
		buf.Reset()
		BufferPool.Put(buf)
	}
}
