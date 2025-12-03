// internal/worker/timecache.go
package worker

import (
	"sync/atomic"
	"time"
)

//
// timecache.go
// ------------------------------------------------------------
// 매초(time.Now 호출 비용을 줄이기 위해) 현재 UTC epoch seconds,
// 그리고 KST 기준 날짜/시간 파티션을 캐싱하는 모듈.
//
// ingest 서버는 초당 수천~수만 개의 이벤트를 처리하므로,
// 매 이벤트마다 time.Now() 호출하면 불필요한 시스템 콜 증가.
// 따라서 1초 ticker로 캐싱 후 초단위 정밀도만 유지한다.
//
// 사용처:
//   - Event.Ts (수집시각)
//   - S3 파티션 prefix (dt=YYYY-MM-DD / hr=HH)
// ------------------------------------------------------------

var (
	// 이벤트 timestamp(UTC epoch seconds)
	unixSec atomic.Int64

	// 날짜/시간 파티션(KST 기준)
	dtVal atomic.Value // "YYYY-MM-DD"
	hrVal atomic.Value // "HH"
)

const kstOffset = 9 * time.Hour

func init() {
	// 최초 seed
	seed()

	// 1초마다 갱신
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for range ticker.C {
			update()
		}
	}()
}

// 최초 시점 초기화
func seed() {
	now := time.Now()
	unixSec.Store(now.Unix())

	kst := now.Add(kstOffset)
	dtVal.Store(kst.Format("2006-01-02"))
	hrVal.Store(kst.Format("15"))
}

// 매초 업데이트
func update() {
	now := time.Now()
	unixSec.Store(now.Unix())

	kst := now.Add(kstOffset)
	dtVal.Store(kst.Format("2006-01-02"))
	hrVal.Store(kst.Format("15"))
}

// ------------------------------------------------------------
// Public API
// ------------------------------------------------------------

// Unix returns current UTC epoch seconds (cached, 1-second precision).
func Unix() int64 {
	return unixSec.Load()
}

// DT returns "YYYY-MM-DD" (KST 기준).
func DT() string {
	return dtVal.Load().(string)
}

// HR returns "HH" (KST 기준).
func HR() string {
	return hrVal.Load().(string)
}
