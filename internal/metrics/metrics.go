package metrics

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Metrics 는 서버 상태를 나타내는 카운터 모음이다.
type Metrics struct {
    // ======================
    // HTTP 레벨 지표
    // ======================

    // HTTPRequestsTotal
    // - /collect 엔드포인트로 들어온 "모든" HTTP 요청 수 (시도 기준).
    // - 메서드(GET/POST/OPTIONS) 및 성공/실패/413/503 여부와 관계없이,
    //   HandleCollect 진입마다 1씩 증가한다.
    // - 트래픽 규모(초당 요청 수)를 가장 단순하게 파악하는 지표.
    HTTPRequestsTotal int64

    // HTTPRequestsAcceptedTotal
    // - EventCh(ingest 파이프라인)로 성공적으로 enqueue 된 요청 수.
    // - 즉, 내부 배치/업로드 파이프라인에 "정상적으로 들어간" 요청만 센다.
    // - HTTPRequestsTotal - HTTPRequestsAcceptedTotal 을 보면,
    //   바디 초과(413), 큐 full(503) 등으로 "수집에 실패한 요청" 규모를 간접적으로 알 수 있다.
    HTTPRequestsAcceptedTotal int64

    // HTTPRequestsRejectedBodyTooLargeTotal
    // - 요청 Body 가 MaxBodySize 를 초과해서 거절된(413 반환) 요청 수.
    // - MaxBytesReader 및 io.Copy 시 에러가 발생한 케이스에서 증가한다.
    // - upstream 이 비정상적으로 큰 payload 를 보내는지, 설정된 MaxBodySize 가 현실적인지 확인하는 용도.
    // - 비율: 이 값 / HTTPRequestsTotal → "body 사이즈 정책 위반 비율".
    HTTPRequestsRejectedBodyTooLargeTotal int64

    // HTTPRequestsRejectedQueueFullTotal
    // - 내부 EventCh 큐가 가득 차서 즉시 503(Service Unavailable)을 반환한 요청 수.
    // - Fail-Fast backpressure 가 실제로 몇 번 동작했는지 알 수 있다.
    // - 이 값이 지속적으로 증가하면:
    //   1) ingest 서버가 처리 용량을 초과했거나,
    //   2) 업로드(S3, DLQ) 단계가 느려져서 큐가 자주 막히고 있다는 신호.
    // - 비율: 이 값 / HTTPRequestsTotal → "시스템 과부하로 인한 드랍 비율".
    HTTPRequestsRejectedQueueFullTotal int64

    // ======================
    // S3 레벨 지표
    // ======================

    // S3EventsStoredTotal
    // - 최종적으로 S3에 "성공 저장된 이벤트 개수"를 나타낸다.
    // - 단위는 "이벤트 수"이며, "배치 수"가 아니다.
    //   예: 100개 이벤트로 이루어진 배치 1개 업로드 성공 → +100.
    // - RAW prefix 로 정상 저장된 이벤트만 센다 (RAW_DLQ 는 별도).
    // - 이 값이 계속 증가하는지 / 멈춰있는지로, 수집 파이프라인이 실제로 S3에 데이터를 쌓고 있는지 판단할 수 있다.
    S3EventsStoredTotal int64

    // S3PutErrorsTotal
    // - S3 PutObject 호출이 실패한 "시도(attempt)" 횟수.
    // - retry 가 있다면, 한 번의 업로드 작업에서도 여러 번 증가할 수 있다.
    //   예: 3회 재시도 설정, 모두 실패 → 이 카운터는 +3.
    // - 네트워크 문제, S3 일시 장애, 권한 문제 등 "API 호출 레벨 에러"를 감시하는 용도.
    // - S3EventsStoredTotal 과 함께 보면:
    //   - S3PutErrorsTotal 이 거의 0에 가깝다면 정상.
    //   - 갑자기 이 값이 튀면 S3/API 실패가 증가했다는 의미.
    S3PutErrorsTotal int64

    // ======================
    // DLQ (Dead Letter Queue) 지표
    // ======================

    // DLQEventsEnqueuedTotal
    // - DLQ 에 들어간 "이벤트 수"의 누적 합.
    // - 로컬 DLQ 파일 하나에는 여러 이벤트가 gzip+JSONL 형태로 들어가는데,
    //   그 배치에 포함된 이벤트 개수를 모두 합산해서 카운트한다.
    // - 어떤 종류의 실패로 인해 DLQ 를 거치게 되는지에 따라 해석:
    //   - S3 업로드 실패로 인해 로컬 DLQ 에 저장된 이벤트 수.
    //   - (encode 실패는 별도로 raw_dlq 로 바로 보낸다면, 이 카운터에는 포함되지 않음.)
    // - 이 값이 크다는 것은 "주 수집 경로에서 처리하지 못한 이벤트를 DLQ 로 우회시킨" 양이 많다는 뜻.
    DLQEventsEnqueuedTotal int64

    // DLQEventsReuploadedTotal
    // - DLQ 에 있던 데이터를 다시 S3 로 재업로드하는 과정에서 "성공적으로 복구된" 이벤트 수.
    // - 현재 구현에서는 배치 단위로만 파일 정보를 알 수 있기 때문에,
    //   엄밀한 이벤트 수 추적이 어려운 경우, 배치 1건을 1 이벤트로 취급할 수도 있다.
    //   (정확한 이벤트 수를 알고 싶다면 메타데이터를 추가로 저장해야 함.)
    // - DLQEventsEnqueuedTotal 과 비교해서:
    //   - 높으면 "DLQ에 쌓였던 것 대부분이 결국 복구되었다"는 뜻,
    //   - 낮으면 아직 처리되지 못한 DLQ backlog 가 많다는 뜻.
    DLQEventsReuploadedTotal int64

    // DLQEventsDroppedTotal
    // - DLQ 가 용량 제한(DLQMaxSizeBytes)에 걸려서 "새로 들어올 이벤트를 버린" 횟수.
    // - Save 시점에 ensureCapacity 가 실패해서 더 이상 파일을 추가할 수 없을 때 증가한다.
    // - 이 값이 0이 아닌 것은 이미 DLQ 용량 정책을 초과해 **데이터를 영구적으로 잃기 시작했다**는 강한 신호.
    // - 비율 예: DLQEventsDroppedTotal / DLQEventsEnqueuedTotal → 
    //   DLQ 자체에서도 감당 못 하고 버리는 비율.
    DLQEventsDroppedTotal int64

    // DLQFilesExpiredTotal
    // - TTL(DLQMaxAge) 또는 용량 제한에 의해 삭제된 DLQ 파일 수.
    // - "오래되어서 버린 것" 또는 "새 데이터를 위해 공간을 만들기 위해 제거한 것"의 누적 개수.
    // - DLQEventsDroppedTotal 이 "새 데이터를 받지도 못하고 버린" 사례라면,
    //   DLQFilesExpiredTotal 은 "기존에 저장되었던 파일을 정책에 따라 청소한" 사례.
    DLQFilesExpiredTotal int64

    // DLQFilesCurrent
    // - 현재 로컬 DLQ 디렉토리에 존재하는 파일 개수.
    // - gauge 형식 값이며, 프로세스 시작 시 디렉토리를 스캔해서 초기화되고,
    //   파일 추가/삭제 시마다 증가/감소한다.
    // - 운영 중에 이 값이 계속 증가만 한다면:
    //   - DLQ 파일이 재업로드/삭제되지 않고 쌓이기만 한다는 뜻 → DLQ 처리(loop)가 제대로 돌고 있는지 확인 필요.
    DLQFilesCurrent int64

    // DLQSizeBytes
    // - 현재 로컬 DLQ 디렉토리의 전체 용량(bytes 단위).
    // - 파일 쓰기/삭제 시마다 해당 파일의 크기만큼 증가/감소시켜 추적한다.
    // - DLQMaxSizeBytes 설정 대비 어느 정도 찼는지 모니터링 가능.
    //   예: DLQSizeBytes / DLQMaxSizeBytes → 현재 사용률.
    // - DLQSizeBytes 가 Max 에 근접한 상태에서 DLQEventsDroppedTotal 이 증가하기 시작하면,
    //   DLQ 용량을 늘리거나, DLQ 처리 속도를 높이거나, 근본적인 실패 원인을 줄이는 대응이 필요하다.
    DLQSizeBytes int64
}

func New() *Metrics {
	return &Metrics{}
}

func (m *Metrics) String() string {
	var sb strings.Builder
	sb.Grow(256)

	fmt.Fprintf(&sb, "http_requests_total=%d\n", atomic.LoadInt64(&m.HTTPRequestsTotal))
	fmt.Fprintf(&sb, "http_requests_accepted_total=%d\n", atomic.LoadInt64(&m.HTTPRequestsAcceptedTotal))
	fmt.Fprintf(&sb, "http_requests_rejected_body_too_large_total=%d\n", atomic.LoadInt64(&m.HTTPRequestsRejectedBodyTooLargeTotal))
	fmt.Fprintf(&sb, "http_requests_rejected_queue_full_total=%d\n", atomic.LoadInt64(&m.HTTPRequestsRejectedQueueFullTotal))

	fmt.Fprintf(&sb, "s3_events_stored_total=%d\n", atomic.LoadInt64(&m.S3EventsStoredTotal))
	fmt.Fprintf(&sb, "s3_put_errors_total=%d\n", atomic.LoadInt64(&m.S3PutErrorsTotal))

	fmt.Fprintf(&sb, "dlq_events_enqueued_total=%d\n", atomic.LoadInt64(&m.DLQEventsEnqueuedTotal))
	fmt.Fprintf(&sb, "dlq_events_reuploaded_total=%d\n", atomic.LoadInt64(&m.DLQEventsReuploadedTotal))
	fmt.Fprintf(&sb, "dlq_events_dropped_total=%d\n", atomic.LoadInt64(&m.DLQEventsDroppedTotal))
	fmt.Fprintf(&sb, "dlq_files_expired_total=%d\n", atomic.LoadInt64(&m.DLQFilesExpiredTotal))
	fmt.Fprintf(&sb, "dlq_files_current=%d\n", atomic.LoadInt64(&m.DLQFilesCurrent))
	fmt.Fprintf(&sb, "dlq_size_bytes=%d\n", atomic.LoadInt64(&m.DLQSizeBytes))

	return sb.String()
}