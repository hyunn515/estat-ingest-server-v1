Architecture — Estat Ingest Server v1

본 문서는 시스템 전반의 구조와 설계 의도를 심층적으로 설명합니다.

⸻

1. Top-Level 개요

flowchart LR
    subgraph HTTP Layer
        A1[HTTP Handler]
    end

    subgraph Internal Pipeline
        A2[Event Pool]
        A3[EventCh]
        A4[CollectLoop]
        A5[UploadCh]
        A6[UploadLoop]
    end

    subgraph Worker Components
        A7[Encoder (gzip + JSONL)]
        A8[S3Uploader]
        A9[DLQ Manager]
    end

    Client --> A1
    A1 --> A3
    A3 --> A4
    A4 --> A5
    A5 --> A6

    A6 --> A7
    A7 --> A8
    A8 -->|Fail| A9
    A9 -->|Retry| A8
    A8 -->|Success| S3[(AWS S3)]


⸻

2. 구성요소 설명

2.1 HTTP Handler
	•	/collect 요청 처리
	•	JSON 파싱 후 EventPool에서 객체 재사용
	•	EventCh에 push
	•	QueueFull 시 503 반환

2.2 Event Channel

비동기 파이프라인의 핵심. 요청 처리 속도와 업로드 처리 속도를 decouple.

2.3 CollectLoop
	•	BatchSize 또는 FlushInterval 조건 충족 시 UploadCh에 job push

2.4 UploadLoop
	•	Gzip 인코딩
	•	S3 업로드
	•	실패 시 DLQ 저장
	•	Idle 시 DLQ 복구

2.5 DLQ Manager
	•	로컬 디스크 저장
	•	Partial Scan 기반 Retry
	•	TTL/용량 제한 관리

⸻

3. Shutdown 시퀀스

sequenceDiagram
    participant CLoop as CollectLoop
    participant ULoop as UploadLoop

    CLoop->>ULoop: Close uploadCh
    ULoop->>ULoop: Drain jobs
    ULoop-->>Main: Completed

Shutdown은 Drain Pattern을 적용하여 유실을 방지합니다.

⸻