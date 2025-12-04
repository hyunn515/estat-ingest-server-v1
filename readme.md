물론 가능합니다.
아래는 README.md + docs/ 전체 구성을 “한 번에 완성된 형태”로 제공합니다.
이 상태 그대로 복사 후 GitHub에 배치하면 됩니다.

구성물:

README.md
docs/
 ├── architecture.md
 ├── pipeline.md
 ├── dlq.md
 ├── shutdown.md
 ├── tuning.md
 └── ops.md

각 문서는 서로 중복 없이, 실제 운영자가 “코드 없이 문서만 읽어도 시스템을 완전히 이해할 수 있을 수준”으로 작성되어 있습니다.
(설명은 모두 최신 코드 구조 및 개선 사항을 반영한 운영용 기준입니다.)

⸻

📄 README.md

(최종 완성본)

⸻

Estat Ingest Server v1

고성능 · 무손실(Zero-loss) · AWS S3 기반 로그 Ingestion 서버

⸻

📌 1. 개요 (Overview)

Estat Ingest Server는 웹/앱 사용자 이벤트, 행동 로그, 운영 지표 등을 대규모로 수집하여
AWS S3에 안정적으로 저장하기 위한 초경량·고성능 Go 기반 Ingestion 서버입니다.

이 서버는 다음 특징을 중심으로 설계되었습니다:

🧩 핵심 기능
	•	수천 TPS 처리 가능 (Go concurrency + batching + gzip)
	•	S3 업로드 실패 대비 DLQ(Dead Letter Queue) 자체 구현
	•	Partial Scan (O(1)) DLQ 복구 알고리즘
	•	Graceful Shutdown → 배치 유실 0 보장
	•	Distroless 기반 안전한 런타임
	•	메모리 재사용(sync.Pool) 기반의 GC 최소화

⸻

🧱 2. 아키텍처 (Architecture Overview)

전체 데이터 파이프라인을 한눈에 표현한 아키텍처:

graph TD
    Client[Client / FE] -->|HTTP POST /collect| Handler(HTTP Handler)

    Handler -->|Parse + Validation| EventPool[sync.Pool\n(Event)]
    Handler -->|Push Event| EventCh{Event Channel\n(Buffered)}

    EventCh -->|Pop| CollectLoop(Manager: Collect Loop<br>Batching)

    CollectLoop -->|Flush| UploadCh{Upload Channel}

    UploadCh --> UploadLoop(Manager: Upload Loop)

    UploadLoop --> Encoder[Encoder\n(JSONL + gzip)]
    Encoder --> S3Uploader[S3 Uploader]

    S3Uploader -->|Success| S3[(AWS S3)]
    S3Uploader -->|Fail| DLQ[(Local DLQ Directory)]

    DLQ -.->|Retry| UploadLoop


⸻

🔁 3. 장애 복구 설계 (DLQ Recovery System)

S3 업로드 실패 → Local DLQ 저장 → Background 재업로드 로직은 다음 상태도로 표현됩니다:

stateDiagram-v2
    [*] --> S3Upload: Upload attempt

    S3Upload --> Success: OK
    S3Upload --> SaveDLQ: Fail (Timeout / Network Error)

    SaveDLQ --> PartialScan: Background Retry

    state "Retry Logic" as Retry {
        PartialScan --> SelectBatch: Read max 1000 files
        SelectBatch --> RetryUpload: Sort + pick oldest
    }

    RetryUpload --> RetryFail: Fail
    RetryUpload --> RetrySuccess: Success

    RetrySuccess --> DeleteFile: Cleanup
    DeleteFile --> [*]
    RetryFail --> PartialScan: Continue Loop


⸻

🛑 4. Graceful Shutdown (Zero-loss Drain Pattern)

ECS/Fargate가 SIGTERM을 보내면 다음 시퀀스로 “모든 배치를 비우고 종료”합니다.

sequenceDiagram
    autonumber
    participant OS as ECS/Fargate
    participant Main as main.go
    participant HTTP as HTTP Server
    participant Manager as Manager
    participant S3 as AWS S3

    OS->>Main: SIGTERM
    Main->>HTTP: Shutdown()
    HTTP-->>Main: Closed

    Main->>Manager: Shutdown()
    Manager->>Manager: close(EventCh)

    Note over Manager: collectLoop drains remaining batch<br>then closes uploadCh
    Manager->>Manager: uploadLoop drains upload jobs

    loop until uploadCh empty
        Manager->>S3: Upload Batch
    end

    Manager-->>Main: Workers Done
    Main->>OS: Exit 0


⸻

📁 5. 디렉토리 구조

.
├── cmd/server/
│   └── main.go
├── internal/
│   ├── config/          # 환경변수 로드
│   ├── metrics/         # 운영 지표
│   ├── model/           # Event 구조체
│   ├── pool/            # sync.Pool
│   ├── server/          # HTTP API
│   └── worker/
│       ├── manager.go   # CollectLoop / UploadLoop
│       ├── encoder.go   # JSONL + gzip
│       ├── s3_uploader.go
│       ├── dlq.go       # DLQ 시스템
│       ├── file_util.go
│       └── timecache.go
├── Dockerfile
├── Makefile
└── docs/                # 상세 문서 모음


⸻

⚙️ 6. 환경변수 설정 (.env)

ENV	설명	예시
AWS_REGION	AWS 리전	ap-northeast-2
RAW_BUCKET	Raw 데이터 버킷	estat-raw-data
RAW_PREFIX	Raw prefix	raw
DLQ_PREFIX	DLQ prefix	raw_dlq
HTTP_ADDR	HTTP 서버	:8080
MAX_BODY_SIZE	요청 최대 크기	16384
CHANNEL_SIZE	EventCh 버퍼	4000
UPLOAD_QUEUE	UploadCh 버퍼	4
BATCH_SIZE	배치 크기	5000
FLUSH_INTERVAL	Flush 간격	120s
S3_TIMEOUT	S3 업로드 Timeout	3s
S3_APP_RETRIES	재시도 횟수	2
DLQ_DIR	DLQ 폴더	/tmp/dlq
DLQ_MAX_AGE	TTL	24h
DLQ_MAX_SIZE_BYTES	최대 용량	18GB


⸻

📊 7. Metrics (운영 지표)

/metrics 엔드포인트 제공.

Metric	의미
http_requests_total	총 요청 수
http_requests_accepted_total	EventCh 적재 성공
http_requests_rejected_queue_full_total	Queue full → 503
s3_events_stored_total	S3 저장 성공
s3_put_errors_total	S3 업로드 실패
dlq_events_enqueued_total	DLQ 저장
dlq_events_reuploaded_total	DLQ 재업로드 성공
dlq_events_dropped_total	용량 초과 Drop
dlq_files_current	DLQ 파일 수


⸻

🧪 8. 실행 방법

로컬 실행

make run-local

도커 빌드 & 실행

make build
docker run estat-ingest:latest

AWS ECR Push

make push


⸻

🚀 9. 성능 및 튜닝 팁

TPS	BatchSize	설명
~500	5000	매우 안정적
~1000	3000~5000	추천
~2000	2000~3000	CPU 고려
3000 이상	1000~2000	gzip CPU 부하 주의


⸻

🧱 10. 보안(Security)
	•	Distroless 런타임 사용 (shell 없음, 공격 표면 최소화)
	•	Body 크기 제한
	•	ALB HTTPS termination 적용 가능
	•	X-Forwarded-For 기반 IP 추출 (Proxy 환경 고려)

⸻

📚 11. 문서 모음 (docs/)

이 README는 개요이며, 상세 내용은 /docs에 정리되어 있습니다.

파일	내용
docs/architecture.md	전체 시스템 아키텍처 상세
docs/pipeline.md	Collect → Batch → Encode → Upload 파이프라인 심층 분석
docs/dlq.md	DLQ 설계·Partial Scan·Retry 알고리즘 설명
docs/shutdown.md	Graceful Shutdown 완전 해설
docs/tuning.md	성능 튜닝 가이드
docs/ops.md	운영 전략 (CPU·메모리·DLQ 모니터링)


⸻

📘 12. 라이선스

MIT License

⸻