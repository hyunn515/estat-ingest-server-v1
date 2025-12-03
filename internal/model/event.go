// internal/model/event.go
package model

// Event
// ------------------------------------------------------------
// 클라이언트로부터 수집된 단일 로그 이벤트 구조체.
// ingestion 파이프라인에서 모든 데이터의 "기본 단위"가 된다.
// Handler → Manager → Encoder → S3 업로드까지 그대로 전달된다.
//
// Body 필드는 사용자가 전송한 raw query/body 문자열이며,
// 쿠키/유저에이전트 등 함께 수집한 부가 정보는
// downstream ETL 단계에서 분리·정제하게 된다.
type Event struct {
	Ts        int64  `json:"ts"`         // Event 수집 시각 (UTC epoch seconds) — timecache.Unix() 기반
	IP        string `json:"ip"`         // 실 사용자 IP (ALB/XFF/CF 헤더 기반 추출)
	UserAgent string `json:"user_agent"` // User-Agent 문자열
	Cookie    string `json:"cookie"`     // Cookie header raw string
	Body      string `json:"body"`       // GET: RawQuery / POST: Body text
}

// UploadJob
// ------------------------------------------------------------
// 이벤트 배치 단위로 업로드할 때 Manager 내부에서 사용되는 구조체.
// Encoder → gzip JSONL → S3Uploader 로 전달된다.
type UploadJob struct {
	Events []*Event // 한 번에 처리되는 N개의 이벤트
}
