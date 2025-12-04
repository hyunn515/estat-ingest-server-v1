Graceful Shutdown — Zero Loss Guarantee

⸻

1. 핵심 원칙
	•	cancel()을 먼저 부르지 않는다.
	•	EventCh → CollectLoop → UploadCh → UploadLoop 순으로 닫는다.

⸻

2. 시퀀스

sequenceDiagram
    autonumber
    participant M as Manager
    participant C as CollectLoop
    participant U as UploadLoop

    M->>C: close(EventCh)
    C->>C: flush batch
    C->>U: close(uploadCh)
    U->>U: drain jobs
    U-->>M: done


⸻
