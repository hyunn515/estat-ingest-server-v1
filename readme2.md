# estat-ingest-server-v1

작은 Go 서버 하나로 **프론트엔드 이벤트를 수집 → S3에 안전하게 적재 → 장애 시 DLQ로 보호**하는 Ingest 서버입니다.  
ECS/Fargate 0.25~0.5 vCPU 환경을 타겟으로, **저비용 + 고안정성**을 목표로 설계되었습니다.

---

## 1. 목표와 특징

### ✅ 목표

- FE에서 보내는 **작은 JSON 이벤트**를 받아서
- **JSONL + gzip** 형식으로 묶어
- **S3 `raw/` 파티션(dt, hr)** 에 저장하고
- **S3 / 네트워크 장애 시에도 데이터를 최대한 잃지 말 것**
- Fargate의 **작은 리소스(0.25~0.5 vCPU, 512MiB 메모리)** 에서 안정적으로 동작할 것

### ✨ 주요 특징

- **배치 처리**
  - `BatchSize` + `FlushInterval` 기반으로 이벤트를 모았다가 업로드
  - 작은 파일 난립(Too many small files)을 줄이려는 시도
- **Backpressure (자연적인 부하 제어)**
  - `EventCh`, `uploadCh` 모두 **bounded channel**
  - 서버가 감당 못 하는 트래픽은 **/collect가 느려지고 타임아웃 → 자연스러운 shedding**
- **DLQ(Dead Letter Queue) 설계**
  - S3 업로드 실패 시 **로컬 디스크(/tmp/dlq)** 에 gzip+JSONL 파일 그대로 저장
  - 별도 루프에서 주기적으로 다시 S3 업로드 시도
  - 오래된 파일/용량 초과 파일 자동 정리
- **Graceful Shutdown**
  - `SIGTERM` 수신 시
    - 더 이상 새 요청은 안 받고
    - 채널을 닫아서 파이프라인을 자연스럽게 비우고
    - 남은 배치까지 업로드 시도 후 종료
- **운영 친화적**
  - `/health`, `/metrics`, 풍부한 로그
  - 환경변수 기반 설정 (`config.Config`)
  - S3 Retry 정책을 **애플리케이션 레벨에서 명시적으로 제어**

---

## 2. 전체 아키텍처 / 흐름 시각화

### 2.1 상위 구조 (파이프라인)

```mermaid
flowchart LR
    subgraph Client Side
        FE[Browser / App]
    end

    subgraph Ingest Server
        H[/HTTP /collect/]
        ECH[EventCh (buffer)]
        C[collectLoop<br/>배치 생성]
        UCH[uploadCh (buffer)]
        U[uploadLoop<br/>인코딩 + 업로드]
        D[DLQManager<br/>로컬 DLQ + 재업로드]
    end

    subgraph S3
        RAW[(s3://RAW_BUCKET/RAW_PREFIX/...)]
        DLQRAW[(s3://RAW_BUCKET/DLQ_PREFIX/...)]
    end

    FE -->|JSON 이벤트 POST| H -->|*model.Event push*| ECH --> C -->|UploadJob| UCH --> U
    U -->|성공| RAW
    U -->|인코딩 실패 → raw_dlq| DLQRAW
    U -->|S3 업로드 실패 → Save| D
    D -->|재업로드 성공| RAW
    D -->|깨진 파일| DLQRAW

2.2 시퀀스 다이어그램

sequenceDiagram
    participant FE as Frontend
    participant S as /collect Handler
    participant M as Manager
    participant CL as collectLoop
    participant UL as uploadLoop
    participant S3 as AWS S3
    participant DLQ as DLQManager

    FE->>S: POST /collect (JSON)
    S->>S: body read, validate, build Event
    S->>M: push Event to EventCh
    Note over S,M: EventCh가 꽉 차면 /collect는<br/>block → FE 타임아웃 → 자연스러운 shedding

    M->>CL: EventCh에서 이벤트 소비
    CL->>CL: BatchSize / FlushInterval 기준으로 batch 생성
    CL->>UL: UploadJob(Events) push to uploadCh

    UL->>UL: EncodeBatchJSONLGZ (JSONL + gzip)
    alt 인코딩 성공
        UL->>S3: PutObject (RAW_PREFIX/dt=.../hr=.../file.jsonl.gz)
        alt 업로드 성공
            UL->>M: S3EventsStoredTotal++
        else 업로드 실패
            UL->>DLQ: Save(data, num_events)
        end
    else 인코딩 실패
        UL->>S3: PutObject (DLQ_PREFIX/..., raw jsonl)
    end

    loop 주기적으로
        UL->>DLQ: ProcessOneCtx() (최대 N회)
        DLQ->>S3: PutObject (RAW_PREFIX 또는 DLQ_PREFIX)
    end


⸻

3. 코드 구조

estat-ingest
├── cmd
│   └── server
│       └── main.go            # 엔트리 포인트 (HTTP 서버, Graceful shutdown)
├── internal
│   ├── config
│   │   └── config.go          # 환경변수 로딩, Config 구조체
│   ├── metrics
│   │   └── metrics.go         # 운영 지표 struct + /metrics 핸들러
│   ├── model
│   │   └── event.go           # Event, UploadJob 정의
│   ├── pool
│   │   └── pool.go            # *Event 재사용용 Object Pool (sync.Pool)
│   ├── server
│   │   ├── handler.go         # /collect, /metrics HTTP 핸들러
│   │   └── ip.go              # X-Forwarded-For에서 클라이언트 IP 추출
│   └── worker
│       ├── manager.go         # 이벤트 파이프라인 코어 (collectLoop, uploadLoop)
│       ├── encoder.go         # JSONL + gzip 인코딩
│       ├── s3_uploader.go     # S3 PutObject + 재시도
│       ├── dlq.go             # 로컬 DLQ + 재업로드
│       ├── file_util.go       # 파일명, S3 키 생성
│       └── timecache.go       # dt, hr, unix timestamp 캐시
├── Dockerfile                 # multi-stage build (distroless)
├── Makefile                   # build / docker / push
├── go.mod
└── go.sum


⸻

4. HTTP 인터페이스

4.1 /collect – 이벤트 수집
	•	Method: POST
	•	Body: JSON (구체 포맷은 유연하게, body 필드에 그대로 문자열로 저장)
	•	제약: MAX_BODY_SIZE (환경변수, 기본 16KB) 초과 시 4xx

핸들러에서 하는 일:
	1.	Content-Length 및 body size 체크
	2.	IP, User-Agent, Cookie 등 메타 추출
	3.	model.Event 생성
	4.	Manager.EventCh로 push
	5.	성공 시: 200 OK, 간단한 JSON 또는 텍스트
	6.	EventCh 꽉 찬 상태로 오래 block → FE 타임아웃 → 더 이상 트래픽 못 받고 자연 감속

4.2 /metrics – 운영 지표

단순 텍스트 포맷으로 내부 카운터를 노출합니다.

대표 항목:
	•	S3EventsStoredTotal : 정상적으로 RAW에 저장된 이벤트 수
	•	S3PutErrorsTotal : S3 PutObject 에러 횟수
	•	DLQSizeBytes : 현재 DLQ 디렉토리 총 사용량
	•	DLQFilesCurrent : 현재 DLQ 파일 개수
	•	DLQEventsEnqueuedTotal : DLQ에 들어간 이벤트 수
	•	DLQEventsReuploadedTotal : DLQ → S3 재업로드 성공 이벤트 수
	•	DLQEventsDroppedTotal : 공간 부족 등으로 버린 이벤트 수

→ CloudWatch Agent나 Prometheus sidecar에서 scrape하여 대시보드 구성 가능.

4.3 /health – 헬스 체크
	•	단순히 "ok" 문자열 반환
	•	ALB Target Group Health Check 용도
	•	서버가 panic, OOM, deadlock 등의 이유로 정상 응답을 못 하면 ALB가 인스턴스를 교체

⸻

5. 구성 / 설정 (Config & Env)

5.1 Config 구조체

type Config struct {
    AWSRegion string

    RawBucket string
    RawPrefix string
    DLQPrefix string

    InstanceID string
    HTTPAddr   string

    MaxBodySize   int64
    ChannelSize   int
    UploadQueue   int
    BatchSize     int
    FlushInterval time.Duration

    S3Timeout    time.Duration
    S3AppRetries int

    DLQDir          string
    DLQMaxAge       time.Duration
    DLQMaxSizeBytes int64
}

5.2 환경 변수 예시 (.env 느낌)

# AWS region
AWS_REGION=ap-northeast-2

# Raw S3
RAW_BUCKET=estat-raw-data
RAW_PREFIX=raw           # 정상 이벤트 파일
DLQ_PREFIX=raw_dlq       # 깨진 파일 / 재시도 실패 파일

# HTTP listen address
HTTP_ADDR=:8080

# Request body max size (bytes)
MAX_BODY_SIZE=16384

# Channel / Batch config
CHANNEL_SIZE=4000        # EventCh 버퍼
UPLOAD_QUEUE=4           # uploadCh 버퍼 (배치 개수)
BATCH_SIZE=5000          # 배치당 이벤트 수
FLUSH_INTERVAL=120s      # 배치 flush 주기

# S3 Upload
S3_TIMEOUT=3s            # 1회 PutObject timeout
S3_APP_RETRIES=2         # 애플리케이션 레벨 재시도 횟수 (총 3회 시도)

# Local DLQ (Fargate ephemeral disk)
DLQ_DIR=/tmp/dlq
DLQ_MAX_AGE=24h          # TTL
DLQ_MAX_SIZE_BYTES=19327352832  # 18GB

⚠️ 주의
	•	S3 Retry는 SDK 내부가 아니라 애플리케이션 레벨(S3_APP_RETRIES) 에서만 제어합니다.
	•	SDK는 RetryMaxAttempts = 0 으로 두어 중복 재시도(Double Retry) 를 방지합니다.

⸻

6. 파이프라인 상세 동작

6.1 collectLoop (배치 생성기)

EventCh (단일 이벤트들)
/collect handler가 푸시
        │
        ▼
[collectLoop]
  - batch에 append
  - 길이가 BatchSize 도달 또는 FlushInterval 경과 시
    → uploadCh에 UploadJob push

핵심 포인트:
	•	Flush 전략
	•	BatchSize ≥ N : 즉시 flush
	•	FlushInterval 경과 : time-based flush
	•	배치 슬라이스 재사용 안 함
	•	flush 시마다 make([]*Event, 0, BatchSize) 새로 생성
	•	encoder가 내부적으로 Event 객체를 풀링하여 메모리 사용량 관리

6.2 uploadLoop (인코딩 + 업로드 + DLQ)

uploadCh (UploadJob)
        │
        ▼
[uploadLoop]
  1) EncodeBatchJSONLGZ (JSONL + gzip)
  2) S3Uploader.UploadBytesWithRetryCtx
     - Timeout: S3_TIMEOUT
     - Retry: S3_APP_RETRIES (App level 루프)
  3) 실패 시 DLQManager.Save()
  4) 매 루프마다 DLQ.ProcessOneCtx() 몇 번 실행 (재업로드)


⸻

7. DLQ 설계

7.1 언제 DLQ에 들어가나?
	1.	JSONL+gzip 인코딩 실패
	•	매우 드문 케이스
	•	원본 이벤트 body들을 줄바꿈 단위로 이어붙여 raw_dlq 에 바로 업로드 (S3)
	•	이 경우 로컬 DLQ 디렉토리는 사용하지 않음
	2.	S3 업로드 실패
	•	네트워크 장애, 권한 문제, S3 일시적 장애, Timeout 등
	•	정상적으로 인코딩된 gzip+JSONL 을 그대로 로컬 DLQ 디렉토리에 저장
	•	이후 별도 루프에서 재시도

7.2 DLQ 저장 방식
	•	데이터 파일:
<unix>_<instance>_<counter>.jsonl.gz
	•	메타 파일:
<unix>_<instance>_<counter>.jsonl.gz.meta.json

예:

/tmp/dlq/
  1701670000_ip-10-0-0-1_000001.jsonl.gz
  1701670000_ip-10-0-0-1_000001.jsonl.gz.meta.json

meta.json 예:

{"num_events":5000}

7.3 용량 관리 (ensureCapacity)
	•	새로운 DLQ 파일을 저장하기 전에:
	1.	DLQMaxSizeBytes 초과 여부 확인
	2.	초과 시 가장 오래된 data 파일부터 삭제
	3.	더 이상 지울 파일이 없으면 → 신규 batch drop (메트릭 DLQEventsDroppedTotal 증가)

7.4 재업로드 (ProcessOneCtx)
	1.	pickOldest() 로 가장 오래된(또는 상위 1000개 중 가장 오래된) 파일명을 선택
	2.	gzip 해제 + 첫 줄 JSON 파싱
	•	성공: RAW_PREFIX 로 재업로드
	•	실패: DLQ_PREFIX 로 업로드 (깨진 파일)
	3.	성공 시 로컬 파일 + meta 삭제
	4.	TTL(DLQMaxAge) 초과 시에는 재업로드 시도 대신 삭제

💡 pickOldest()는 성능을 위해 전체 디렉토리가 아니라 최대 1000개만 읽어 정렬하는 Partial Scan 전략을 사용합니다.
DLQ가 1만, 10만 개까지 쌓여도, 스캔 비용은 상수 시간(O(1)에 가까운)으로 유지되도록 설계했습니다.

⸻

8. Graceful Shutdown 동작 개념

Fargate에서 SIGTERM을 받았을 때, 서버는 다음 순서로 종료를 진행합니다.
	1.	main.go
	•	OS 시그널 수신 (SIGTERM, SIGINT)
	•	http.Server.Shutdown(ctx) 호출 → 더 이상 새 HTTP 요청 수신 안 함
	2.	핸들러 종료
	•	이미 들어와 있던 /collect 요청들은 처리 완료
	3.	Manager.Shutdown()
	•	EventCh를 닫아 collectLoop에게 “더 이상 신규 이벤트 없음”을 알림
	•	collectLoop:
	•	EventCh drain
	•	남은 batch flush → uploadCh
	•	uploadCh 닫기
	•	uploadLoop:
	•	uploadCh에 남아 있는 모든 job 처리
	•	DLQ 재업로드 루프 마무리
	•	WaitGroup 으로 모든 goroutine 종료 기다린 후 cancel()로 Context 정리

즉, “파이프라인 입구를 닫고 → 내부 버퍼를 끝까지 비우고 → 종료” 하는 구조입니다.

⸻

9. 빌드 & 실행

9.1 로컬 빌드

go build -o estat-server ./cmd/server

9.2 로컬 실행

export $(cat .env | grep -v '^#' | xargs)  # 필요하면
./estat-server

9.3 Docker 빌드 (대략 형태)

docker build -t estat-ingest:local .
docker run --rm -p 8080:8080 --env-file .env estat-ingest:local

Dockerfile은 multi-stage build를 사용하여
golang:1.22-bookworm 빌더 → gcr.io/distroless/base-debian12 런타임으로
작고, 안전한 단일 바이너리 컨테이너를 생성합니다.

⸻

10. 튜닝 가이드 (초기 운영용)

10.1 트래픽 수준에 따른 권장 값 (감각치)
	•	TPS ~ 500
	•	BATCH_SIZE = 5000
	•	FLUSH_INTERVAL = 120s
	•	Fargate 0.25 ~ 0.5 vCPU
	•	TPS 500 ~ 2000
	•	실제 CPU 사용률을 보고 조정
	•	CPU가 자주 80% 이상 가면:
	•	BATCH_SIZE를 1000~2000으로 낮추고
	•	Fargate vCPU를 0.5 이상으로 상향 고려

10.2 모니터링해야 할 지표
	•	/metrics에서:
	•	S3EventsStoredTotal 증가 추이
	•	S3PutErrorsTotal 급증 여부
	•	DLQFilesCurrent / DLQSizeBytes
	•	DLQEventsDroppedTotal (절대 0 이상으로 올라가면 안 좋음)
	•	인프라 측:
	•	CPU 사용률
	•	메모리 사용률
	•	ALB 5xx 비율

특히 DLQFilesCurrent가 1000 이상 올라가면 CloudWatch Alarm을 걸어
운영자가 개입할 수 있도록 하는 것을 권장합니다.

⸻

11. 한 줄 요약

이 서버는 “적당히 빠르고, 대충 잘 되는” 게 아니라
“리소스가 빡빡해도, 장애가 떠도, 최대한 데이터를 안 잃고 버티는” 쪽에 더 가깝게 설계된 작은 Ingest 엔진입니다.

이 README를 기반으로 설계 문서/운영 매뉴얼/발표 자료까지 확장해도 좋습니다.
추가로 Mermaid 시퀀스 다이어그램이나 더 세분화된 컴포넌트 다이어그램이 필요하면 바로 만들어 붙일 수 있습니다.

