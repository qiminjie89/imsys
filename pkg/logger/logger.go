// Package logger 提供结构化日志功能
package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var defaultLogger *zap.Logger

// Config 日志配置
type Config struct {
	Level  string // debug, info, warn, error
	Format string // json, console
	Output string // stdout, file path
}

// Init 初始化日志
func Init(cfg Config) error {
	level := parseLevel(cfg.Level)

	var encoder zapcore.Encoder
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "ts"
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	if cfg.Format == "console" {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	}

	var writer zapcore.WriteSyncer
	if cfg.Output == "stdout" || cfg.Output == "" {
		writer = zapcore.AddSync(os.Stdout)
	} else {
		file, err := os.OpenFile(cfg.Output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		writer = zapcore.AddSync(file)
	}

	core := zapcore.NewCore(encoder, writer, level)
	defaultLogger = zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))

	return nil
}

func parseLevel(level string) zapcore.Level {
	switch level {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

// L 返回默认 logger
func L() *zap.Logger {
	if defaultLogger == nil {
		defaultLogger, _ = zap.NewProduction()
	}
	return defaultLogger
}

// Debug 输出 debug 日志
func Debug(msg string, fields ...zap.Field) {
	L().Debug(msg, fields...)
}

// Info 输出 info 日志
func Info(msg string, fields ...zap.Field) {
	L().Info(msg, fields...)
}

// Warn 输出 warn 日志
func Warn(msg string, fields ...zap.Field) {
	L().Warn(msg, fields...)
}

// Error 输出 error 日志
func Error(msg string, fields ...zap.Field) {
	L().Error(msg, fields...)
}

// With 创建带字段的 logger
func With(fields ...zap.Field) *zap.Logger {
	return L().With(fields...)
}

// Sync 刷新日志缓冲
func Sync() error {
	return L().Sync()
}
