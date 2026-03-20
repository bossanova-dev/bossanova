package plugin

import (
	"fmt"
	"io"
	"log"

	"github.com/hashicorp/go-hclog"
	"github.com/rs/zerolog"
)

// hclogAdapter bridges hashicorp/go-hclog to zerolog so that go-plugin
// log output flows through the daemon's structured logger.
type hclogAdapter struct {
	logger zerolog.Logger
	name   string
	args   []any
}

func newHCLogAdapter(logger zerolog.Logger) hclog.Logger {
	return &hclogAdapter{logger: logger}
}

func (h *hclogAdapter) log(level zerolog.Level, msg string, args ...any) {
	e := h.logger.WithLevel(level)
	for i := 0; i+1 < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			key = fmt.Sprint(args[i])
		}
		e = e.Interface(key, args[i+1])
	}
	e.Msg(msg)
}

func (h *hclogAdapter) Trace(msg string, args ...any) {
	h.log(zerolog.TraceLevel, msg, args...)
}

func (h *hclogAdapter) Debug(msg string, args ...any) {
	h.log(zerolog.DebugLevel, msg, args...)
}

func (h *hclogAdapter) Info(msg string, args ...any) {
	h.log(zerolog.InfoLevel, msg, args...)
}

func (h *hclogAdapter) Warn(msg string, args ...any) {
	h.log(zerolog.WarnLevel, msg, args...)
}

func (h *hclogAdapter) Error(msg string, args ...any) {
	h.log(zerolog.ErrorLevel, msg, args...)
}

func (h *hclogAdapter) IsTrace() bool { return h.logger.GetLevel() <= zerolog.TraceLevel }
func (h *hclogAdapter) IsDebug() bool { return h.logger.GetLevel() <= zerolog.DebugLevel }
func (h *hclogAdapter) IsInfo() bool  { return h.logger.GetLevel() <= zerolog.InfoLevel }
func (h *hclogAdapter) IsWarn() bool  { return h.logger.GetLevel() <= zerolog.WarnLevel }
func (h *hclogAdapter) IsError() bool { return h.logger.GetLevel() <= zerolog.ErrorLevel }

func (h *hclogAdapter) With(args ...any) hclog.Logger {
	return &hclogAdapter{
		logger: h.logger,
		name:   h.name,
		args:   append(h.args, args...),
	}
}

func (h *hclogAdapter) Named(name string) hclog.Logger {
	newName := name
	if h.name != "" {
		newName = h.name + "." + name
	}
	return &hclogAdapter{
		logger: h.logger.With().Str("subsystem", newName).Logger(),
		name:   newName,
		args:   h.args,
	}
}

func (h *hclogAdapter) ResetNamed(name string) hclog.Logger {
	return &hclogAdapter{
		logger: h.logger.With().Str("subsystem", name).Logger(),
		name:   name,
		args:   h.args,
	}
}

func (h *hclogAdapter) Name() string                                            { return h.name }
func (h *hclogAdapter) SetLevel(hclog.Level)                                    {}
func (h *hclogAdapter) GetLevel() hclog.Level                                   { return hclog.Debug }
func (h *hclogAdapter) StandardLogger(*hclog.StandardLoggerOptions) *log.Logger { return nil }
func (h *hclogAdapter) StandardWriter(*hclog.StandardLoggerOptions) io.Writer   { return io.Discard }
func (h *hclogAdapter) ImpliedArgs() []any                                      { return h.args }
func (h *hclogAdapter) Log(level hclog.Level, msg string, args ...any) {
	switch level {
	case hclog.Trace:
		h.Trace(msg, args...)
	case hclog.Debug:
		h.Debug(msg, args...)
	case hclog.Info:
		h.Info(msg, args...)
	case hclog.Warn:
		h.Warn(msg, args...)
	case hclog.Error:
		h.Error(msg, args...)
	case hclog.NoLevel, hclog.Off:
		// Nothing to log.
	}
}
