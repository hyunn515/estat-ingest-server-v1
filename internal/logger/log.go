// internal/logger/log.go
package logger

import (
	"io"
	"os"
	"strings"

	"estat-ingest/internal/config"

	stdlog "log"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

// Init
//
// 애플리케이션 시작 시 한 번만 호출되는 로거 초기화 함수입니다.
// Config 설정(환경변수)에 따라 '개발자용 화면' 또는 '운영용 시스템 로그'로
// 자동으로 형태를 바꾸어 설정합니다.
//
// [주요 기능]
//
//  1. 로그 포맷 자동 전환:
//     - 개발 환경 (LOG_PRETTY=true): 알록달록한 텍스트로 출력 (가독성 위주)
//     - 운영 환경 (LOG_PRETTY=false): JSON 포맷으로 출력 (CloudWatch 등 검색/분석 위주)
//
//  2. 공통 필드 자동 추가:
//     - 모든 로그에 "service", "instance" 정보가 자동으로 붙습니다.
//     - 예: {"service":"estat", "instance":"i-123", "msg":"..."}
//     - 서버가 여러 대일 때 어느 서버의 로그인지 즉시 식별 가능합니다.
//
//  3. 로그 샘플링 (비용 절감):
//     - Debug/Info 레벨은 설정에 따라 일부만 기록하고 버립니다 (예: 1/100만 기록).
//     - Warn/Error(장애 상황)는 절대 버리지 않고 100% 기록합니다.
//
// 사용 예:
//
//	logger.Init(cfg)
//	log.Info().Msg("서버가 시작되었습니다")
func Init(cfg config.Config) {

	// -------------------------------------------------------------------
	// 1) 로그 레벨 결정 (최소 출력 기준)
	// -------------------------------------------------------------------
	// 설정된 레벨보다 낮은 중요도의 로그는 아예 출력하지 않습니다.
	// 예: "info"로 설정하면 "debug" 로그는 무시됩니다.
	level := zerolog.InfoLevel
	if l, err := zerolog.ParseLevel(strings.ToLower(strings.TrimSpace(cfg.LogLevel))); err == nil {
		level = l
	}

	zerolog.SetGlobalLevel(level)

	// -------------------------------------------------------------------
	// 2) 출력 방식 결정 (사람 vs 기계)
	// -------------------------------------------------------------------
	// 여기서 로그가 화면에 어떻게 그려질지를 결정합니다.
	var w io.Writer

	if cfg.LogPretty {
		// [Local 개발 환경]
		// 사람이 눈으로 터미널을 볼 때 편하도록 색상과 정렬을 적용합니다.
		// 예: 10:00:05 INF 서버 시작됨 service=estat
		w = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: "15:04:05", // 개발 중엔 날짜 없이 시간만 보여도 충분함
		}
	} else {
		// [Prod 운영 환경]
		// AWS CloudWatch, Datadog 같은 시스템이 자동으로 분석하기 가장 좋은
		// '표준 JSON' 포맷을 그대로 내보냅니다.
		// 별도의 가공 없이 Raw Data를 os.Stdout으로 흘려보냅니다.
		// 예: {"time":"...","level":"info","message":"서버 시작됨","service":"estat"}
		w = os.Stdout
	}

	// -------------------------------------------------------------------
	// 3) 기본 Logger 생성 (공통 태그 부착)
	// -------------------------------------------------------------------
	// 위에서 결정한 출력 방식(w)을 기반으로 로거를 만듭니다.
	// 모든 로그에 서비스명과 인스턴스ID를 꼬리표처럼 항상 붙입니다.
	base := zerolog.New(w).
		Level(level).
		With().
		Timestamp().                     // 언제 발생했는지 시간 기록
		Str("service", cfg.ServiceName). // 어떤 서비스인지 (예: estat-ingest)
		Str("instance", cfg.InstanceID). // 어떤 서버(컨테이너)인지
		Logger()

	// -------------------------------------------------------------------
	// 4) 샘플링 설정 (로그 홍수 방지 & 비용 절감)
	// -------------------------------------------------------------------
	// 트래픽이 많을 때 Info/Debug 로그가 너무 많이 쌓이면 비용이 됩니다.
	// 중요도가 낮은 로그는 N개 중 1개만 남기고 나머지는 버립니다.
	logger := base

	if cfg.LogSampleN > 1 {
		logger = base.Sample(&zerolog.LevelSampler{
			// Debug/Info: 설정된 N값에 따라 확률적으로 기록 (예: N=100이면 1%만 기록)
			DebugSampler: &zerolog.BasicSampler{N: cfg.LogSampleN},
			InfoSampler:  &zerolog.BasicSampler{N: cfg.LogSampleN},

			// Warn/Error: 샘플링하지 않음 (nil).
			// 장애나 경고는 하나도 빠짐없이 모두 기록해야 원인을 찾을 수 있습니다.
		})
	}

	// -------------------------------------------------------------------
	// 5) 전역 Logger 교체
	// -------------------------------------------------------------------
	// 이제부터 프로그램 어디서든 log.Info(), log.Error()를 쓰면
	// 위에서 설정한 규칙대로 로그가 남습니다.
	zlog.Logger = logger

	// Go 언어의 기본 라이브러리(log.Println 등)를 쓰더라도
	// 우리가 만든 zerolog 설정을 따르도록 연결해줍니다.
	stdlog.SetFlags(0)            // zerolog가 시간을 따로 찍으므로 기본 시간 포맷 제거
	stdlog.SetOutput(zlog.Logger) // 표준 로그의 출력 방향을 zerolog로 돌림
}
