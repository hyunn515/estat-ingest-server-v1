// internal/worker/file_util.go
package worker

import (
	"fmt"
	"sync/atomic"
)

// file_util.go
// ------------------------------------------------------------
// DLQ 및 RAW 파일 저장 시 사용하는 유틸리티 모음.
// 파일명 규칙은 ingest 전체 파이프라인의 정렬·TTL 판단의 핵심이므로
// 예측 가능한 deterministic 패턴을 유지해야 한다.
//
// 파일명 규칙:
//
//	<unix>_<instance>_<counter>.jsonl.gz
//
// 예:
//
//	1764721594_ingest1_000042.jsonl.gz
//
// 정렬하면 곧 시간 순 정렬이므로,
// DLQ에서 파일을 다시 업로드할 때 가장 오래된 파일 선처리에 사용한다.
var globalCounter uint64

// NextCounter
// ------------------------------------------------------------
// 원자적 증가 값으로 여러 goroutine에서 충돌 없이
// 순차 번호를 생성한다.
// 1,000,000(약 1e6)에서 다시 0으로 돌아가므로 파일명이 지나치게 커지는 것을 방지.
// wrap-around 되어도 timestamp·instance ID 조합으로
// 전체 파일명 충돌 가능성은 사실상 0에 가깝다.
func NextCounter() uint64 {
	return atomic.AddUint64(&globalCounter, 1) % 1_000_000
}

// NewFilename
// ------------------------------------------------------------
// 새로운 파일명을 생성한다.
// <unix>_<instance>_<counter>.jsonl.gz 형태.
//
// DLQ 및 RAW 모두 동일 패턴을 사용해도 무방하며,
// prefix 계층은 BuildS3Key에서 적용한다.
func NewFilename(instanceID string) string {
	sec := Unix()
	c := NextCounter()
	return fmt.Sprintf("%d_%s_%06d.jsonl.gz", sec, instanceID, c)
}

// BuildS3Key
// ------------------------------------------------------------
// 표준화된 S3 Key 생성기.
// S3 폴더 구조(Partitioning):
//
//	<prefix>/dt=<YYYY-MM-DD>/hr=<HH>/<filename>
//
// Athena / Glue 파티션 스캔 비용을 줄이기 위한 표준적인 구조.
// ingest 내부에서는 prefix(RAW, DLQ)만 구분해 주면 된다.
func BuildS3Key(prefix, filename string) string {
	return fmt.Sprintf("%s/dt=%s/hr=%s/%s", prefix, DT(), HR(), filename)
}
