# 🛑 Graceful Shutdown Strategy

ECS/Fargate의 스케일인(Scale-in)이나 배포 시, **처리 중이던 데이터를 단 하나도 잃지 않고(Zero-loss)** 종료하기 위한 **Drain Pattern**입니다.

---

## Shutdown Sequence

```mermaid
sequenceDiagram
    autonumber
    participant OS as OS (SIGTERM)
    participant Main as main.go
    participant Manager as Manager
    participant CLoop as CollectLoop
    participant ULoop as UploadLoop
    participant S3 as AWS S3

    Note over OS, Main: 1. 종료 신호 감지
    OS->>Main: SIGTERM Signal
    
    Note over Main: 2. 신규 유입 차단
    Main->>Manager: Shutdown() 호출
    Manager->>Manager: close(EventCh)
    
    Note over CLoop: 3. 내부 버퍼 비우기 (Drain)
    CLoop->>CLoop: 채널에 남은 이벤트 처리
    CLoop->>ULoop: Flush remaining Batch
    CLoop->>ULoop: close(UploadCh)
    
    Note over ULoop: 4. 최종 업로드
    loop Until UploadCh Empty
        ULoop->>S3: Upload Last Job
    end
    
    ULoop-->>Manager: Loop Exit (Done)
    Manager-->>Main: wg.Wait() Return
    
    Note over Main, OS: 5. 프로세스 종료
    Main->>OS: Exit(0)
```

## 핵심 원칙
1.  **입구 먼저 닫기**: `EventCh`를 먼저 닫아 더 이상 물이 들어오지 않게 합니다.
2.  **강제 종료 금지**: `Context.Cancel()`을 바로 호출하지 않고, 내부 고루틴들이 하던 일을 마칠 때까지(`sync.WaitGroup`) 기다립니다.
3.  **순차적 종료**: `CollectLoop`가 끝나야 `UploadLoop`가 끝나는 의존 관계를 가집니다.