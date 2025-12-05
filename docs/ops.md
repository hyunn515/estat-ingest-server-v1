# 🔭 Operational Guide (운영 매뉴얼)

이 문서는 Estat Ingest Server를 **운영 환경에서 안정적으로 유지하기 위한 기준**을 정리한 문서입니다.  
운영자가 시스템의 상태를 빠르게 판단하고, 장애를 조기에 감지하며, 필요한 조치를 즉시 수행할 수 있도록 구성되었습니다.

본 문서는 다음과 함께 보면 전체 구조가 완성된다:  
`architecture.md`, `pipeline.md`, `dlq.md`, `shutdown.md`, `tuning.md`

---

# 1. 📡 핵심 모니터링 지표 (Key Metrics & Alert Rules)

운영에서 가장 중요한 것은 **"DLQ 증가 여부"**, **"백프레셔 발생 시점"**, **"S3 업로드 실패율"**이다.  
아래 지표는 모두 알람 조건 설정 대상이며, 최소한 CloudWatch 또는 Prometheus Alertmanager와 연동해야 한다.

---

## 🟥 A. DLQ 관련 (가장 중요)

| Metric | 위험 기준 | 의미 | 즉시 대응 |
|-------|-----------|-------|-----------|
| **`dlq_files_current`** | **> 1000** | S3 업로드 실패가 누적되고 있으며, DLQ 복구 속도가 따라가지 못하는 상황 | 1) S3 latency 확인<br>2) 네트워크·IAM 문제 확인<br>3) 업로드 오류 로그 점검 |
| **`dlq_events_dropped_total`** | **> 0** (절대 발생 금지) | DLQ 디스크 용량 부족 → **데이터 영구 유실 발생** | 1) 디스크 용량 즉시 확보<br>2) DLQ 정책 재점검<br>3) 근본 장애 해결 |

### 운영 기준 설명

- **단순 상승이 아니라 증가 속도(rate)가 더 중요한 신호**  
  예: *초당 20개 증가* → S3 업로드가 현저히 실패 중이며 즉각 원인 파악 필요

- DLQ는 장애 안정장치이지만, **지속적으로 증가하는 상황은 시스템이 정상 동작하고 있지 않다는 강력한 신호**이다.

---

## 🟧 B. HTTP 수집 단계 지표

| Metric | 의미 | 위험 기준 | 대응 |
|--------|------|-----------|------|
| **`http_requests_rejected_queue_full_total`** | EventCh 백프레셔 → 수집량 대비 처리량 부족 | 증가 추세 | 1) Task scale-out<br>2) Upload 지연 원인(S3, gzip) 확인<br>3) BatchSize 조정 |
| **`http_requests_rejected_body_too_large_total`** | 비정상적으로 큰 요청 수신 | 갑작스러운 폭증 | 클라이언트 SDK 문제 또는 공격 여부 확인 |

### 설명
EventCh가 막혀서 거절(throttle)하고 있다는 것은 **Upload 단계 또는 gzip 인코딩이 병목**일 가능성이 높다.

---

## 🟨 C. S3 업로드 단계 지표

| Metric | 의미 | 위험 기준 | 즉시 대응 |
|--------|------|-----------|-----------|
| **`s3_put_errors_total`** | S3 업로드 실패율 | 급증 | 1) S3 서비스 상태 점검<br>2) IAM AccessDenied 여부 확인<br>3) 네트워크 지연 증가 여부 체크 |
| **`s3_events_stored_total`** | 정상 업로드 성공 이벤트 수 | 증가 멈춤 | 파이프라인 동작 중단 가능 → DLQ 증가 여부 병행 확인 |

---

# 2. ⚠️ 대표 장애 시나리오 & 분석 가이드

아래는 운영 중 실제로 발생할 수 있는 장애를  
**증상 → 원인 후보 → 조치** 3단 구조로 재정리했다.

---

## 🟥 시나리오 1: DLQ가 비정상적으로 빠르게 증가 (`dlq_files_current ↑`)

### 증상
- DLQ 파일이 초당 수십 개씩 증가
- S3 events stored 증가 속도 감소 또는 멈춤
- 업로드 로그에 오류 다수 발생

### 원인 후보
1. S3 PutObject latency 상승 또는 장애
2. VPC Endpoint 성능 저하
3. IAM AccessDenied 또는 권한 변경
4. Fargate task 네트워크 지연 또는 일시 장애

### 조치
- DLQ 디렉토리 용량 확인  
- 장애 해결 전 **절대 배포하지 않을 것**  
- DLQ가 5000개 이상으로 증가할 경우 Scale-out 검토  
- `s3_put_errors_total` 로그 패턴 분석  
- AWS Status 페이지 또는 CloudWatch S3 API latency 점검

---

## 🟧 시나리오 2: CPU 사용률 90% 이상 유지

### 증상
- gzip 인코딩 단계가 밀리기 시작함  
- UploadLoop 처리 속도 저하 → UploadCh 적체  
- EventCh까지 영향을 주며 503 증가

### 원인
- BatchSize가 과도하게 높음  
- 0.25 vCPU 환경에서 gzip 압축 비용이 예상보다 큼  
- DLQ Reupload 작업이 잦아 UploadLoop가 CPU를 독점

### 대응
1. BatchSize 조정 (`5000 → 3000 또는 2000`)  
2. Task CPU 증가 (0.25 → 0.5 vCPU)  
3. Task 수 증가 (scale-out)  
4. gzip 레벨 변경은 지원하지 않음 → BatchSize 조절이 가장 효과적

---

## 🟨 시나리오 3: 503 응답 증가 및 queue_full 발생

### 증상
- 프론트엔드 또는 SDK에서 503 error 다수 발생
- server 로그: `queue full (drop)` 또는 유사 메시지
- metrics: `http_requests_rejected_queue_full_total` 증가

### 원인
- UploadLoop 처리 속도 < 수집 속도  
- EventCh가 backlog를 처리하지 못함  
- DLQ 엔트리가 많아 UploadLoop idle 재시도가 잦아짐  
  (※ 현재 UploadLoop idle 시에는 **단 1건만 DLQ 처리**하므로 영향은 제한적)

### 대응
1. UploadCh backlog 확인  
2. CPU / gzip 압축 병목 여부 파악  
3. Task scale-out  
4. DLQ 증가 추이 병행 확인  
5. BatchSize 조정으로 처리 속도 균형 맞추기

---

# 3. 🔎 로그 패턴 해석 (Log Interpretation)

운영 중 로그 패턴은 장애 진단에 매우 중요한 단서다.

### 정상 패턴
```
[INFO] DLQ → RAW success: raw/2025/... events=5000
```
→ 손상되지 않은 DLQ 파일이 정상적으로 복구됨  
→ UploadLoop idle 시 1건씩 처리되고 있다는 의미  

---

### 경고 패턴
```
[WARN] DLQ capacity → removed=1700000039332.jsonl.gz
```
→ TTL 또는 용량 제한으로 삭제가 발생함  
→ DLQ 증가 속도가 빠르다는 신호일 수 있음  
→ S3 latency 또는 권한 문제 병행 점검 필요  

---

### 오류 패턴
```
[ERROR] local DLQ save failed: no space left on device
```
→ DLQ 디스크 Full  
→ **데이터 유실 발생 중 (중대한 장애)**  
→ 즉시 디스크 용량 확보 또는 DLQ 비우기 필요  

---

# 4. 🧭 운영 체크리스트

운영자는 다음 항목을 정기적으로 확인해야 한다.

---

## 📌 배포 전
- `/metrics` 엔드포인트 정상 노출  
- DLQ 디렉토리 비정상 파일 여부 확인  
- S3 API latency 정상 여부 확인  
- 현재 DLQ 파일 증가 추이가 안정적인지  

---

## 📌 일일 점검
- DLQ 증가 속도 (`dlq_files_current`, `delta rate`)
- `s3_events_stored_total` 정상 증가 여부  
- HTTP Rejected(QueueFull, BodyTooLarge) 발생 여부  
- CPU 사용량 급상승 여부  
- S3 PutError 패턴  

---

## 📌 장애 발생 후
- DLQ 복구 성공률(`dlq_events_reuploaded_total`) 확인  
- 삭제된 파일 수(`dlq_files_expired_total`) 확인  
- S3 API latency 원인 분석  
- DLQ 증가 기간 동안 유실(`dlq_events_dropped_total`)이 있었는지 확인  

---

# 5. 📌 결론

운영 매뉴얼의 목적은 다음 두 가지이다:

1) **장애를 최대한 빨리 감지할 것**  
2) **장애가 어느 계층에서 발생했는지 즉시 판단할 수 있을 것**  

이를 위해 운영자는 다음 질문에 빠르게 답할 수 있어야 한다:

- S3 업로드가 지연되고 있는가?  
- EventCh 또는 UploadCh 어느 쪽에서 병목이 발생했는가?  
- DLQ 용량이 위험 수준인가?  
- DLQ 복구 루프가 정상 동작하고 있는가?  
- CPU 병목(gzip)이 발생하고 있는가?  

이 문서는 시스템 전체를 안정적인 상태로 유지하기 위한 운영 기준이며,  
`architecture.md`, `dlq.md`, `pipeline.md`, `shutdown.md`, `tuning.md`  
문서와 함께 사용하면 전체 구조·동작·장애 대응 전략까지 완전하게 이해할 수 있다.