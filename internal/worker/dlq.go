// internal/worker/dlq.go
package worker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"estat-ingest/internal/config"
	"estat-ingest/internal/metrics"

	json "github.com/goccy/go-json"
	"github.com/klauspost/compress/gzip"
)

// DLQManager 는 S3 업로드 실패 배치를 로컬 디스크에 저장하고,
// 이후 재업로드를 담당한다.
// - encode 실패: 바로 S3 raw_dlq 로 업로드 (여기 안 옴)
// - S3 업로드 실패: gzip+JSONL 배치를 로컬 DLQ에 저장
// TTL 판단은 "파일명 prefix 의 Unix timestamp" 기준으로 한다.
type DLQManager struct {
	cfg      config.Config
	metrics  *metrics.Metrics
	uploader *S3Uploader

	// 현재 DLQ 디렉토리에 저장된 data 파일 총 바이트 수
	dlqSizeBytes int64
}

// NewDLQManager 는 DLQ 디렉토리를 초기화하고, 기존 파일을 스캔하여
// DLQSizeBytes / DLQFilesCurrent 를 복원한다.
// 이때 meta orphan (data 없이 .meta.json 만 남은 경우) 도 정리한다.
func NewDLQManager(cfg config.Config, m *metrics.Metrics, uploader *S3Uploader) *DLQManager {
	_ = os.MkdirAll(cfg.DLQDir, 0o755)

	d := &DLQManager{
		cfg:      cfg,
		metrics:  m,
		uploader: uploader,
	}

	var total int64
	var count int64

	entries, err := os.ReadDir(cfg.DLQDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}

			name := e.Name()
			full := filepath.Join(cfg.DLQDir, name)

			// meta orphan 제거: *.meta.json 이고, 같은 이름의 data 파일이 없으면 삭제
			if strings.HasSuffix(name, ".meta.json") {
				dataName := strings.TrimSuffix(name, ".meta.json")
				if _, err := os.Stat(filepath.Join(cfg.DLQDir, dataName)); os.IsNotExist(err) {
					_ = os.Remove(full)
				}
				continue
			}

			// data 파일만 카운트
			info, err := e.Info()
			if err == nil {
				total += info.Size()
				count++
			}
		}
	}

	atomic.StoreInt64(&d.dlqSizeBytes, total)
	if total > 0 {
		atomic.AddInt64(&m.DLQSizeBytes, total)
	}
	if count > 0 {
		atomic.AddInt64(&m.DLQFilesCurrent, count)
	}

	return d
}

// Save 는 S3 업로드 실패한 gzip+JSONL 배치를 로컬 DLQ 에 저장한다.
// numEvents 는 해당 배치에 포함된 이벤트 수이며, 메타 파일(.meta.json)에 기록된다.
//
// TTL 판단은 파일명 prefix 의 Unix timestamp 기반이므로
// 별도로 mtime 을 조정할 필요는 없다.
func (d *DLQManager) Save(data []byte, numEvents int) error {
	if len(data) == 0 || numEvents <= 0 {
		return nil
	}

	size := int64(len(data))
	if !d.ensureCapacity(size) {
		// 용량 부족: 가장 오래된 파일들 정리했지만 여전히 공간 부족 → drop
		log.Printf("[ERROR] DLQ full → drop bytes=%d events=%d", size, numEvents)
		atomic.AddInt64(&d.metrics.DLQEventsDroppedTotal, int64(numEvents))
		return nil
	}

	filename := NewFilename(d.cfg.InstanceID)         // "<unix>_<instance>_<counter>.jsonl.gz"
	dataPath := filepath.Join(d.cfg.DLQDir, filename) // data 파일
	metaPath := dataPath + ".meta.json"               // 메타 파일

	// data 파일 저장
	if err := os.WriteFile(dataPath, data, 0o600); err != nil {
		return err
	}

	// 메타 파일 저장 (현재는 num_events 만 기록)
	meta := []byte(fmt.Sprintf(`{"num_events":%d}`, numEvents))
	_ = os.WriteFile(metaPath, meta, 0o600)

	// metrics
	atomic.AddInt64(&d.dlqSizeBytes, size)
	atomic.AddInt64(&d.metrics.DLQSizeBytes, size)
	atomic.AddInt64(&d.metrics.DLQFilesCurrent, 1)
	atomic.AddInt64(&d.metrics.DLQEventsEnqueuedTotal, int64(numEvents))

	return nil
}

// ensureCapacity 는 DLQMaxSizeBytes 를 초과하지 않도록
// 가장 오래된 data/meta 파일부터 삭제한다.
// data 파일이 더 이상 없으면 false 를 반환한다.
func (d *DLQManager) ensureCapacity(incoming int64) bool {
	max := d.cfg.DLQMaxSizeBytes
	if max <= 0 {
		return true
	}

	for {
		curr := atomic.LoadInt64(&d.dlqSizeBytes)
		if curr+incoming <= max {
			return true
		}

		oldest := d.pickOldest()
		if oldest == "" {
			return false
		}

		dataPath := filepath.Join(d.cfg.DLQDir, oldest)
		metaPath := dataPath + ".meta.json"

		info, err := os.Stat(dataPath)
		if err == nil {
			atomic.AddInt64(&d.dlqSizeBytes, -info.Size())
			atomic.AddInt64(&d.metrics.DLQSizeBytes, -info.Size())
		}

		_ = os.Remove(dataPath)
		_ = os.Remove(metaPath)

		atomic.AddInt64(&d.metrics.DLQFilesCurrent, -1)
		atomic.AddInt64(&d.metrics.DLQFilesExpiredTotal, 1)

		log.Printf("[WARN] DLQ capacity → removed=%s", oldest)
	}
}

// ProcessOneCtx 는 가장 오래된 data/meta 파일 1개를 RAW 또는 RAW_DLQ 로 재업로드한다.
// TTL 판단도 여기에서 수행한다.
// TTL 기준은 파일명 prefix 의 Unix timestamp 이며, worker.Unix() 기준으로 비교한다.
func (d *DLQManager) ProcessOneCtx(ctx context.Context) {
	// shutdown 신호 체크
	select {
	case <-ctx.Done():
		return
	default:
	}

	name := d.pickOldest()
	if name == "" {
		return
	}

	dataPath := filepath.Join(d.cfg.DLQDir, name)
	metaPath := dataPath + ".meta.json"

	info, err := os.Stat(dataPath)
	if err != nil {
		// 파일이 사라진 경우 정리만 수행
		_ = os.Remove(dataPath)
		_ = os.Remove(metaPath)
		atomic.AddInt64(&d.metrics.DLQFilesCurrent, -1)
		return
	}

	size := info.Size()

	// --- TTL 판단: 파일명 prefix 의 Unix timestamp 기반 ---
	if d.cfg.DLQMaxAge > 0 {
		if sec, ok := extractUnixFromFilename(name); ok && sec > 0 {
			nowSec := Unix() // worker timecache (epoch seconds)
			age := time.Duration(nowSec-sec) * time.Second
			if age > d.cfg.DLQMaxAge {
				// TTL 초과 → 삭제
				_ = os.Remove(dataPath)
				_ = os.Remove(metaPath)

				atomic.AddInt64(&d.dlqSizeBytes, -size)
				atomic.AddInt64(&d.metrics.DLQSizeBytes, -size)
				atomic.AddInt64(&d.metrics.DLQFilesCurrent, -1)
				atomic.AddInt64(&d.metrics.DLQFilesExpiredTotal, 1)

				log.Printf("[INFO] DLQ TTL expired → deleted=%s age=%s", name, age.String())
				return
			}
		}
		// filename 에서 unix 를 읽지 못하면 TTL 판단은 skip 하고 계속 진행
	}

	// shutdown 다시 체크
	select {
	case <-ctx.Done():
		return
	default:
	}

	// data 파일 open
	f, err := os.Open(dataPath)
	if err != nil {
		log.Printf("[WARN] DLQ open failed: %s err=%v", name, err)
		return
	}
	defer f.Close()

	// gzip+JSONL 파일 유효성 검사 (첫 라인 JSON 확인)
	valid := d.validateFile(f, size)

	// 재업로드 전에 rewind
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		log.Printf("[WARN] DLQ seek failed: %s err=%v", name, err)
		return
	}

	// 유효하면 RAW, 아니면 RAW_DLQ 로 보낸다.
	var key string
	if valid {
		key = BuildS3Key(d.cfg.RawPrefix, name)
	} else {
		key = BuildS3Key(d.cfg.DLQPrefix, name)
	}

	if err := d.uploader.UploadFileWithRetryCtx(ctx, key, f, size); err != nil {
		log.Printf("[WARN] DLQ reupload failed: %s err=%v", key, err)
		return
	}

	// meta 에서 num_events 읽기 (없거나 깨져 있으면 1 로 fallback)
	numEvents := int64(1)
	if meta, err := os.ReadFile(metaPath); err == nil {
		var v struct {
			NumEvents int64 `json:"num_events"`
		}
		if json.Unmarshal(meta, &v) == nil && v.NumEvents > 0 {
			numEvents = v.NumEvents
		}
	}

	// 업로드 성공 → 로컬 파일 제거
	_ = os.Remove(dataPath)
	_ = os.Remove(metaPath)

	atomic.AddInt64(&d.dlqSizeBytes, -size)
	atomic.AddInt64(&d.metrics.DLQSizeBytes, -size)
	atomic.AddInt64(&d.metrics.DLQFilesCurrent, -1)
	atomic.AddInt64(&d.metrics.DLQEventsReuploadedTotal, numEvents)

	if valid {
		log.Printf("[INFO] DLQ → RAW success: %s events=%d", key, numEvents)
	} else {
		log.Printf("[INFO] DLQ → RAW_DLQ success: %s events=%d", key, numEvents)
	}
}

// validateFile 은 gzip 을 풀어 첫 번째 JSONL 라인이 유효한 JSON 인지 검사한다.
// 유효하면 RAW 로, 아니면 RAW_DLQ 로 보낸다.
func (d *DLQManager) validateFile(f *os.File, size int64) bool {
	if size <= 0 {
		return false
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}

	gz, err := gzip.NewReader(f)
	if err != nil {
		return false
	}
	defer gz.Close()

	reader := bufio.NewReader(gz)
	line, err := reader.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return false
	}

	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return false
	}

	var tmp map[string]interface{}
	return json.Unmarshal(line, &tmp) == nil
}

// pickOldest는 DLQ 디렉토리에 있는 데이터 파일들 중,
// "부분 스캔(partial scan)"을 이용해 가장 오래된 파일을 선택한다.
//
// ------------------------------------------------------------
// [운영 최적화: Partial Scan 방식 적용]
// ------------------------------------------------------------
// 장애 상황(예: S3 장애)에서 DLQ 파일이 수천~수만 개까지 쌓일 수 있다.
// 기존의 전체 스캔(ReadDir → 전체 정렬) 방식은 O(N log N) 특성 때문에
// 파일 수가 많아지면 CPU/I/O가 폭증하여 ingest 서버 전체가 응답을 못하게 된다.
//
// 이를 방지하기 위해 다음 전략을 사용한다:
//
//  1. 디렉토리에서 최대 1,000개만 읽어온다 (Readdirnames).
//     - 디렉토리 전체 크기(N)에 관계없이 항상 일정한 비용으로 동작한다.
//     - 실제 파일 수가 1천개든 10만개든 처리 비용은 동일(상수 시간).
//
//  2. 이 1,000개(또는 그 이하)의 후보군만 정렬한다.
//     - 1천개 이하 정렬 비용은 미미하므로 CPU 부담이 없다.
//
//  3. 이 후보군에서 가장 오래된 파일을 선택한다.
//     - 파일명에 timestamp가 포함되므로 문자열 정렬로 시간순 정렬이 가능함.
//
// 운영적 판단:
//   - DLQ는 "가능하면 재업로드, 아니면 TTL 지나면 삭제"가 목적이므로
//     전통적 의미의 완전한 FIFO가 아니라도 충분하다.
//   - 파일 삭제/추가로 인해 디렉토리 엔트리 순서는 지속적으로 변하므로,
//     partial scan만으로도 장기적으로 모든 파일이 처리될 가능성이 높다.
//   - 장애 상황에서도 ingest 서버의 안정성을 최우선으로 보장하는 설계이다.
//
// ------------------------------------------------------------
func (d *DLQManager) pickOldest() string {
	// 1. 디렉토리 열기
	f, err := os.Open(d.cfg.DLQDir)
	if err != nil {
		return ""
	}
	defer f.Close()

	// 2. Partial Scan: 최대 1,000개의 파일명만 읽어온다.
	// - 전체를 다 읽지 않으므로 O(K) 파일 스캔 성능을 보장한다.
	// - Readdirnames는 빈 문자열을 반환하지 않으므로 별도의 빈 값 검사는 불필요하다.
	names, err := f.Readdirnames(1000)
	if err != nil && len(names) == 0 {
		// 읽을 파일이 없거나(EOF), 디렉토리 읽기 실패 시
		return ""
	}

	// 3. 유효한 데이터 파일 필터링
	// - .meta.json 파일 제외
	// - 숨김 파일(.으로 시작) 제외
	var candidates []string
	for _, name := range names {
		// Readdirnames는 빈 이름을 반환하지 않으므로 name[0] 접근은 안전하다.
		if strings.HasSuffix(name, ".meta.json") || name[0] == '.' {
			continue
		}
		candidates = append(candidates, name)
	}

	// 유효한 파일이 하나도 없는 경우
	if len(candidates) == 0 {
		return ""
	}

	// 4. 배치 내 정렬 (In-Memory Sort)
	// - 1,000개 이내의 소량 데이터이므로 CPU 비용은 무시할 수 있는 수준이다.
	// - 파일명에 Unix Timestamp가 포함되어 있으므로 문자열 정렬로 시간순 정렬이 된다.
	sort.Strings(candidates)

	// 배치 내에서 가장 오래된 파일 반환
	return candidates[0]
}

// extractUnixFromFilename 은 DLQ 파일명 prefix 에서 Unix seconds 를 파싱한다.
// 파일명 형식: "<unix>_<instance>_<counter>.jsonl.gz"
func extractUnixFromFilename(name string) (int64, bool) {
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return 0, false
	}
	sec, err := strconv.ParseInt(name[:idx], 10, 64)
	if err != nil || sec <= 0 {
		return 0, false
	}
	return sec, true
}
