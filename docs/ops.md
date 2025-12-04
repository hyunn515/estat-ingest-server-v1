Operational Guide (운영 가이드)

⸻

1. 클라우드WATCH 알람 추천

Metric	임계값	조치
dlq_files_current	> 1000	S3 문제 조사
http_requests_rejected_queue_full_total	증가	Scale-out
s3_put_errors_total	증가	네트워크/S3 점검


⸻

2. 로그 관리
	•	slog(JSON) 사용 시 CloudWatch Insights 질의가 쉬워짐

⸻

3. 디스크 관리

DLQ 용량 초과 시 파일 삭제 → 영구 유실
Alarm 필수

⸻