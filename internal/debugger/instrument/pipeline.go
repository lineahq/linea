package instrument

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The Collector creates one processor instance per pipeline but does not put
// the pipeline ID into processor.Settings. It only attaches it to the
// component logger as the "otelcol.pipeline.id" field (service
// componentattribute.LoggerWithAttributes, v0.162.0), which calls With on the
// root logger's core. The debugger supplies that root logger, so a core
// installed with WrapCore sees the field and can answer a probe written
// through the component logger.

const (
	pipelineIDKey = "otelcol.pipeline.id"
	probeMessage  = "linea.pipeline-probe"
	probeKey      = "linea.pipeline-probe"
)

// WrapCore makes a logger able to answer pipelineOf. Install it on the root
// logger handed to the Collector with zap.WrapCore(instrument.WrapCore).
func WrapCore(c zapcore.Core) zapcore.Core { return probeCore{Core: c} }

type probeCore struct {
	zapcore.Core
	pipeline string
}

func (c probeCore) With(fields []zapcore.Field) zapcore.Core {
	pipeline := c.pipeline
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	if v, ok := enc.Fields[pipelineIDKey].(string); ok {
		pipeline = v
	}
	return probeCore{Core: c.Core.With(fields), pipeline: pipeline}
}

func (c probeCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if e.Message == probeMessage {
		return ce.AddCore(e, c)
	}
	return c.Core.Check(e, ce)
}

func (c probeCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	if e.Message != probeMessage {
		return c.Core.Write(e, fields)
	}
	for _, f := range fields {
		if f.Key == probeKey {
			if dst, ok := f.Interface.(*string); ok {
				*dst = c.pipeline
			}
		}
	}
	return nil
}

// pipelineOf returns the pipeline ID attached to a component logger, or ""
// if the logger was not derived from a WrapCore logger. The probe goes to
// the core directly, so it works whatever the logger's level, and it is
// never written to the real log.
func pipelineOf(l *zap.Logger) string {
	if l == nil {
		return ""
	}
	var got string
	ent := zapcore.Entry{Level: zapcore.DebugLevel, Message: probeMessage}
	if ce := l.Core().Check(ent, nil); ce != nil {
		ce.Write(zap.Field{Key: probeKey, Type: zapcore.SkipType, Interface: &got})
	}
	return got
}
