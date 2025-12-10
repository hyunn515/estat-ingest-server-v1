// internal/logger/log.go
package logger

import (
	"os"
	"strings"

	"estat-ingest/internal/config"

	stdlog "log"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

// Init
//
// 애플리케이션 시작 시 한 번만 호출되는 로거 초기화 함수.
//   - Config.LogLevel, LogPretty, LogSampleN 기반으로 zerolog 전역 설정을 구성한다.
//   - 이후에는 "github.com/rs/zerolog/log" 패키지의 log.Debug().Msg(...)
//     등의 API 를 그대로 사용하면 된다.
//
// 특징:
//   - 로그 포맷: JSON(기본) 또는 콘솔 pretty 포맷 (개발 환경)
//   - 공통 필드: service, instance
//   - 샘플링: Info / Debug 레벨에만 적용 (Warn / Error 는 항상 출력)
//
// 사용 예:
//
//	cfg := config.Load()
//	logger.Init(cfg)
//	log.Info().Str("component", "main").Msg("server starting")
func Init(cfg config.Config) {
	// -------------------------------------------------------------------
	// 1) 로그 레벨 결정 (문자열 → zerolog.Level)
	// -------------------------------------------------------------------
	level := zerolog.InfoLevel
	if l, err := zerolog.ParseLevel(strings.ToLower(strings.TrimSpace(cfg.LogLevel))); err == nil {
		level = l
	}

	zerolog.SetGlobalLevel(level)

	// -------------------------------------------------------------------
	// 2) 출력 Writer 결정 (JSON vs Console)
	// -------------------------------------------------------------------
	var w zerolog.ConsoleWriter

	if cfg.LogPretty {
		// 개발 환경용 콘솔 포맷
		w = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: zerolog.TimeFieldFormat, // 기본 포맷 사용 (RFC3339)
		}
	} else {
		// JSON 포맷: ConsoleWriter 를 쓰되 human-readable 포맷만 안 쓰게 구성.
		// 실제로는 ConsoleWriter 없이 New(os.Stdout) 를 써도 되지만,
		// 한 곳에서 포맷 타입을 바꾸기 쉽게 통일한다.
		w = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			NoColor:    true,
			TimeFormat: zerolog.TimeFieldFormat,
		}
	}

	// -------------------------------------------------------------------
	// 3) 기본 Logger 생성 (공통 필드 포함)
	// -------------------------------------------------------------------
	base := zerolog.New(w).
		Level(level).
		With().
		Timestamp().
		Str("service", cfg.ServiceName).
		Str("instance", cfg.InstanceID).
		Logger()

	// -------------------------------------------------------------------
	// 4) 샘플링 설정 (Debug/Info 에만 적용)
	// -------------------------------------------------------------------
	logger := base

	if cfg.LogSampleN > 1 {
		// LevelSampler 를 사용하면 레벨별로 샘플링 전략을 분리할 수 있다.
		// 여기서는 Debug/Info 만 샘플링하고, Warn/Error 는 항상 출력.
		logger = base.Sample(&zerolog.LevelSampler{
			DebugSampler: &zerolog.BasicSampler{N: cfg.LogSampleN},
			InfoSampler:  &zerolog.BasicSampler{N: cfg.LogSampleN},
			// WarnSampler, ErrorSampler 는 nil → 샘플링 없음 (항상 출력)
		})
	}

	// -------------------------------------------------------------------
	// 5) 전역 Logger 교체
	// -------------------------------------------------------------------
	// github.com/rs/zerolog/log 의 전역 Logger 를 우리가 만든 logger 로 교체한다.
	zlog.Logger = logger

	// 표준 라이브러리 log 패키지의 출력도 zerolog 로 보내,
	// 혹시 남아있는 log.Print 계열 호출이 있어도 일관된 포맷으로 수집되게 한다.
	stdlog.SetFlags(0)            // zerolog 에서 타임스탬프를 찍으므로 prefix/flags 제거
	stdlog.SetOutput(zlog.Logger) // zerolog.Logger 는 io.Writer 를 구현한다
}
