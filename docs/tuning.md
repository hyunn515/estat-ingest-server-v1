Performance Tuning Guide

⸻

1. Batch Size

TPS	BatchSize
~500	5000
~1000	3000
~2000	2000
3000↑	1000


⸻

2. UploadQueue
	•	너무 크면 메모리 폭증
	•	4~10 사이 유지 권장

⸻

3. Gzip CPU 부담
	•	0.25 vCPU는 압축이 병목될 수 있음
	•	0.5 vCPU 이상 권장

⸻
