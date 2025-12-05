# 🔭 Operational Guide

서버 상태를 모니터링하고 장애 징후를 조기에 파악하기 위한 가이드입니다.

---

## 1. Key Metrics (필수 모니터링)

`/metrics` 엔드포인트 또는 CloudWatch에서 다음 지표를 반드시 알람으로 설정하세요.

| Metric | 위험 임계값 | 의미 및 조치 |
| :--- | :--- | :--- |
| **`dlq_files_current`** | `> 1000` | **[위험]** DLQ가 해소되지 않고 쌓이고 있음.<br>→ S3 네트워크 상태 확인, 권한 확인 필요. |
| **`http_requests_rejected_queue_full_total`** | `증가 추세` | **[주의]** 처리 용량 한계 도달.<br>→ Fargate Task 개수를 늘리거나(Scale-out) BatchSize 조정. |
| **`s3_put_errors_total`** | `급증` | **[장애]** S3 업로드 실패.<br>→ AWS 상태 대시보드 확인. |
| **`dlq_events_dropped_total`** | `> 0` | **[치명적]** 디스크가 꽉 차서 데이터를 버리기 시작함.<br>→ 즉시 디스크 용량 증설 또는 DLQ 정리 필요. |

---

## 2. Logging Strategy
이 서버는 구조화된 로그(Structured Log, JSON)를 사용하는 것이 좋습니다.

* **정상 로그**: 주기적인 "Batch Uploaded" 로그 (INFO 레벨)
* **에러 로그**: `[ERROR] local DLQ save failed` 같은 문구가 보이면 디스크 상태를 점검해야 합니다.

## 3. Disk Management
Fargate Ephemeral Storage는 용량이 제한적(기본 20GB)입니다.
`DLQ_MAX_SIZE_BYTES` 설정을 통해 디스크가 꽉 차기 전에 오래된 파일을 자동으로 지우도록 설정되어 있으나, 이 경우 데이터 유실이 발생하므로 **`dlq_size_bytes`** 지표를 꾸준히 감시해야 합니다.