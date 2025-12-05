# ⚡ Performance Tuning Guide

서버의 리소스(CPU/Memory)와 유입 트래픽(TPS)에 맞춰 환경 변수를 조정하십시오.

---

## 1. Batch Size & Flush Interval

**배치 크기**는 압축 효율과 직결되며, **Flush 주기**는 데이터의 최신성(Latency)을 결정합니다.

| 예상 TPS | Batch Size | Flush Interval | 설명 |
| :--- | :--- | :--- | :--- |
| **Low (~500)** | `5000` | `120s` | 트래픽이 적을 땐 넉넉하게 모아서 압축 효율을 높입니다. |
| **Mid (~1000)** | `3000` | `60s` | 가장 일반적인 권장 설정입니다. |
| **High (3000+)** | `1000` | `30s` | 배치를 작게 가져가서 CPU 스파이크를 분산시켜야 합니다. |

> **Tip**: `BatchSize`가 너무 크면(10,000+) 압축 시 CPU 점유율이 튀고, 너무 작으면(100) S3 요청 비용이 증가합니다.

---

## 2. Resource Allocation (Fargate)

Gzip 압축은 CPU 집약적인 작업입니다.

* **0.25 vCPU**: TPS 500 이하의 소규모 트래픽에 적합.
* **0.5 vCPU**: TPS 1,000~2,000 구간에서 권장.
* **1.0 vCPU**: TPS 3,000 이상일 때 권장. (Go의 `GOMAXPROCS` 설정 확인 필요)

## 3. Upload Queue
* `UPLOAD_QUEUE` 환경변수는 `UploadLoop`가 처리하기 전 대기하는 배치의 개수입니다.
* **권장값: 4 ~ 10**
* 너무 크면 메모리 사용량이 급증할 수 있습니다. (배치 데이터가 메모리에 상주함)