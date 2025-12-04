Processing Pipeline — Detailed Explanation

⸻


graph TD
    A[HTTP Handler] --> B[Event Validation]
    B --> C[EventCh Push]
    C --> D[CollectLoop]
    D -->|Flush| E[UploadCh]
    E --> F[UploadLoop]
    F --> G[Encoder]
    G --> H[S3Uploader]
    H -->|Fail| I[DLQ]


⸻

1. Collect 단계
	•	Body 크기 제한
	•	JSON 파싱
	•	Pool 기반 Event reuse

2. Batch 단계
	•	BatchSize or FlushInterval
	•	Slice 새로 생성하여 메모리 오염 방지

3. Encode 단계
	•	JSON Lines format
	•	gzip.Writer 재사용

4. Upload 단계
	•	Timeout + App-level Retry
	•	실패 시 DLQ 저장

⸻