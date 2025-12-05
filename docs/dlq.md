# 🔁 Dead Letter Queue (DLQ) Strategy  
**Fail Safe · Controlled Recovery · Predictable Under Failure**

DLQ는 Estat Ingest Server에서 **S3 업로드 실패 시 데이터 유실을 최소화하고**,  
장애가 해소되면 **안정적으로 자동 복구(re-upload)** 하도록 설계된 핵심 컴포넌트입니다.

이 문서는 다음을 다룹니다:

- DLQ 파일 구조  
- 저장/복구 알고리즘  
- Partial Scan(O(K)) 설계 방식  
- TTL 및 용량 정책  
- UploadLoop와의 연계 방식  
- 관측 지표 및 운영 포인트  

---

# 1. DLQ 개요

DLQ의 목적은 두 가지입니다.

## 1.1 Fail Safe — 업로드 실패 시 즉시 안전하게 보관

- S3 업로드 실패 시 **배치 단위로 로컬 디스크(`/tmp/dlq`)에 저장**
- 메타 데이터(`.meta.json`) 포함하여 event count 보존
- 디스크 쓰기까지 실패하는 경우에만 drop  
  → `dlq_events_dropped_total` 증가

## 1.2 Controlled & Predictable Recovery

- UploadLoop가 **idle일 때만 한 번에 1개 파일**을 재처리  
  → 메인 업로드 성능을 방해하지 않음  
  → DLQ 처리량이 UploadLoop의 CPU 점유를 초과하지 않도록 설계
- Partial Scan(O(K)) 기반의 선택 알고리즘으로  
  DLQ 파일 수가 많아도 ingest 서버 부하를 일정하게 유지

---

# 2. DLQ 파일 구조

업로드 실패 시 다음 2개의 파일이 생성됩니다.

```text
<unix>_<instance>_<counter>.jsonl.gz
<unix>_<instance>_<counter>.jsonl.gz.meta.json
```

예:

```text
1700000001_i-abc123_000001.jsonl.gz
1700000001_i-abc123_000001.jsonl.gz.meta.json
```

## 2.1 데이터 파일 (`*.jsonl.gz`)

- gzip 압축된 JSONL
- 각 줄(line)이 하나의 이벤트

예시 (압축 해제 후):

```
{"ts":1700000001,"body":"..."}
{"ts":1700000002,"body":"..."}
```

## 2.2 메타 파일 (`*.meta.json`)

```json
{
  "num_events": 5000
}
```

- 배치 이벤트 개수 보존
- 메타가 없거나 손상된 경우 기본값 1로 간주
- 시작 시 orphan 메타 파일(본체 없이 메타만 있는 파일)은 정리

---

# 3. DLQ 전체 생명 주기

저장 → 용량 검사 → 재시도 → Validation → RAW/RAW_DLQ 업로드 → TTL 관리  
순으로 순환합니다.

```mermaid
flowchart TD

classDef process fill:#F3E5F5,stroke:#4A148C,stroke-width:2px;
classDef decision fill:#FFF3E0,stroke:#E65100,stroke-width:2px;
classDef storage fill:#E1F5FE,stroke:#01579B,stroke-width:2px;
classDef fail fill:#FFEBEE,stroke:#C62828,stroke-width:2px;
classDef success fill:#E8F5E9,stroke:#2E7D32,stroke-width:2px;
classDef trigger fill:#FAFAFA,stroke:#777,stroke-dasharray:5 5;

Fail([S3 Upload Failed]):::fail --> Save[Save Batch<br/>(gzip, jsonl)]:::process
Save --> DLQDir[/Local DLQ<br/>(/tmp/dlq)]:::storage

DLQDir --> CheckCap{"Exceeds Capacity?"}:::decision
CheckCap -->|"Yes"| Evict[Remove Oldest<br/>(Free Space)]:::fail
Evict --> DLQDir
CheckCap -->|"No"| Wait[Idle Until Retry]:::trigger

Wait -.->|"UploadLoop idle"| Scan[Partial Scan<br/>read up to K files]:::process
Scan --> Pick[Pick Oldest]:::process
Pick -->|"Open + Gunzip"| Validate{"Valid JSONL?"}:::decision

Validate -->|"Valid"| UploadRaw[Upload to RAW]:::process
Validate -->|"Corrupted"| UploadBad[Upload to RAW_DLQ]:::fail

UploadRaw -->|"Success"| Del[Delete Local Files]:::success
UploadBad -->|"Success"| Del

UploadRaw -.->|"Fail"| Keep[Keep for Next Retry]:::fail
UploadBad -.->|"Fail"| Keep

Del --> DLQDir
Keep --> DLQDir

DLQDir -.->|"TTL Sweep"| TTL{"Expired?"}:::decision
TTL -->|"Yes"| TTLDel[Delete Expired]:::fail --> DLQDir
TTL -->|"No"| Wait
```

---

# 4. Partial Scan 설계 — O(K) 처리

## 4.1 기존 방식의 문제 (전체 스캔)

만약 매번 전체 DLQ 디렉토리를 스캔한다면:

- 파일이 N개일 때 비용이 O(N) 또는 O(N log N)
- 장애가 지속될수록 DLQ 파일 수가 빠르게 증가  
  → 스캔 시간이 증가  
  → ingest 서버 전체 처리 지연  
  → 심하면 서버가 DLQ 처리 때문에 스스로 병목

## 4.2 Partial Scan(O(K), K=1000) 방식

DLQManager는 매 시도마다 **최대 K개(K=1000)** 파일만 읽습니다.

```go
names, _ := f.Readdirnames(1000) // K=1000
```

그 중:

- 숨김 파일 제외
- `.meta.json` 제외
- 정렬 후 가장 오래된 파일 선택

특징:

- DLQ 파일 개수와 무관하게 처리 비용이 일정 (O(K))
- ingest 서버는 장애 상황에서도 안정적으로 유지
- starvation 가능성은 있으나 K=1000이면 실무적으로 문제 발생 거의 없음  
  (디렉토리 엔트리가 순환하며 결국 모든 파일이 선택됨)

---

# 5. Validation 단계

파일이 유효한 JSONL gzip인지 판별하는 중요한 단계입니다.

DLQ 파일은 다음 조건에서 손상될 수 있습니다:

- gzip footer 기록 전 Task 종료(SIGKILL)
- 디스크 write 실패
- 인코딩 도중 오류 발생

## 5.1 gzip Reader 검증

```go
gz, err := gzip.NewReader(f)
if err != nil {
    return false
}
```

- gzip 헤더/포맷이 손상되면 즉시 실패

## 5.2 JSONL 첫 줄 검사

```go
line, _ := reader.ReadBytes('\n')
if json.Unmarshal(line, &obj) != nil {
    return false
}
```

- JSONL 레코드 구조를 유지하는지 최소 요건 검사  
- 실패 시 RAW_DLQ Prefix로 이동

---

# 6. TTL & 용량 정책

DLQ가 무한히 커지는 것을 방지하기 위해 두 가지 제어 정책을 적용합니다.

## 6.1 TTL 정책 (`DLQ_MAX_AGE`)

- 파일 생성 시간을 파일명 prefix의 timestamp로 계산
- TTL을 초과하면 해당 파일 삭제
- 목적:
  - 복구 불필요한 오래된 데이터 제거
  - 지속적인 디스크 사용량 관리

## 6.2 용량 정책 (`DLQ_MAX_SIZE_BYTES`)

- DLQ 누적 크기가 허용 용량을 초과하면  
  **가장 오래된 DLQ 파일부터 삭제**
- 삭제된 파일 카운트는 `dlq_files_expired_total` 증가

## 6.3 저장 불가 상황(Drop)

- ensureCapacity에서 공간 확보 불가  
  → 배치 자체가 저장되지 않음  
  → `dlq_events_dropped_total` 증가  
  → 이는 **데이터 유실 상황이 이미 발생했음을 의미**

---

# 7. DLQ와 UploadLoop의 연결 방식

## 7.1 업로드 실패 → Save

```go
if err := m.s3.UploadBytesWithRetryCtx(...); err != nil {
    m.dlq.Save(data, len(job.Events))
} else {
    metrics.S3EventsStoredTotal += len(job.Events)
}
```

- 실패한 배치를 DLQManager.Save로 전달
- 저장 후 DLQ 관련 metrics 업데이트

## 7.2 복구 시점 — UploadLoop idle 처리

**DLQ 복구는 UploadLoop의 idle tick에서 1건씩만 수행합니다.**

```go
case <-ticker.C:
    m.dlq.ProcessOneCtx(ctx)  // 1건만 처리
```

설계 근거:

- DLQ를 과도하게 처리하면 UploadLoop CPU 점유가 증가  
  → UploadCh 소비 속도가 느려지고  
  → EventCh → UploadCh로 쌓이는 악순환 발생
- 메인 업로드가 항상 우선이며  
  **DLQ는 '틈날 때' 한 번에 1건씩 처리하는 보조 경로**입니다.

이 방식은 “지속적이고 균일한 복구 속도”를 제공하며,  
장애가 해소되면 DLQ가 점진적으로 비워집니다.

---

# 8. DLQ Metrics

| Metric | 설명 |
|--------|------|
| `dlq_events_enqueued_total` | DLQ에 저장된 이벤트 수 |
| `dlq_events_reuploaded_total` | RAW Prefix로 복구된 이벤트 수 |
| `dlq_events_dropped_total` | 저장 공간 부족 등으로 Drop된 이벤트 수 |
| `dlq_files_expired_total` | TTL/용량 정책으로 삭제된 파일 수 |
| `dlq_files_current` | 현재 DLQ 파일 개수 |
| `dlq_size_bytes` | DLQ 전체 크기 (bytes) |

운영 시 주의해야 할 조건:

- `dlq_events_dropped_total > 0` → **데이터 유실 발생한 상태**
- `dlq_files_current` 지속 증가 → S3 업로드 지연/장애
- `dlq_size_bytes` 임계값 근접 → 용량 증설 또는 장애 원인 분석 필요

---

# 9. 운영 시나리오

## 9.1 S3 장애 발생

- DLQ 파일 지속 증가  
- S3 PutError 증가  
- 장애가 해소되면 idle-time 복구가 자동 진행됨  
- 복구 속도는 UploadLoop CPU 사용률과 비례(1건씩 진행)

## 9.2 디스크 용량 부족

- DLQ Save 실패 로그 발생  
- Drop metrics 증가  
- 즉각적인 운영 조치 필요:
  - DLQ 디렉토리 증설  
  - TTL 정책 단축  
  - 장애 원인(S3/네트워크) 해결

---

# 10. Summary

DLQ는 ingest 서버가 다음을 보장하도록 합니다:

1. **Fail Safe 저장** — 업로드 실패 시 즉시 안전하게 보관  
2. **Controlled Recovery** — idle 시 1건씩 재처리  
3. **Partial Scan(O(K))** — DLQ 규모와 무관하게 일정한 비용  
4. **Validation 기반 재업로드** — 손상 파일은 RAW_DLQ로 분리  
5. **TTL + 용량 관리** — 디스크 무한 증가 방지  
6. **명확한 Metrics** — 장애 조기 탐지 및 운영 단순화

DLQ는 전체 ingest 파이프라인의 신뢰성을 구성하는 중요한 요소이며,  
특히 **S3 장애·네트워크 지연 환경에서도 ingest 서버가 안정적으로 지속 동작하도록 돕는** 보호 계층입니다.