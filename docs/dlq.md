Dead Letter Queue (DLQ) Design

⸻

1. 왜 DLQ인가?

S3 장애는 언제든 발생할 수 있음:
	•	네트워크 단절
	•	AWS Regional Issue
	•	S3 Throttling
	•	SDK Timeout

이때 데이터를 드랍하면 안됨 → DLQ 필요

⸻

2. Partial Scan Algorithm

flowchart TD
    A[Open DLQ Directory] --> B[Readdirnames(1000)]
    B --> C[Filter meta.json / hidden files]
    C --> D[Sort subset]
    D --> E[Pick oldest]
    E --> RetryUpload

장점:
	•	파일 수 무관
	•	항상 O(1) 비용
	•	장애 시에도 서버가 멈추지 않음

⸻

3. Retry Strategy
	•	1000개 중 가장 오래된 파일부터 처리
	•	Idle + uploadLoop에서 매번 3개씩 처리

⸻