package livecaptureextension

import (
	"fmt"
	"regexp"
	"unicode/utf8"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// redactor redacts copies of redacted capture points. A nil redactor, used for
// raw points, leaves them unmodified.
type redactor struct {
	config Redaction
	deny   map[string]bool
	masks  []*regexp.Regexp
}

func newRedactor(config Redaction) (*redactor, error) {
	r := &redactor{config: config, deny: make(map[string]bool)}
	for _, key := range config.DenyAttributes {
		r.deny[key] = true
	}
	if len(config.Masks) > 16 {
		return nil, fmt.Errorf("at most 16 redaction masks are allowed")
	}
	for _, mask := range config.Masks {
		if len(mask) > 256 {
			return nil, fmt.Errorf("redaction regex exceeds 256 bytes")
		}
		re, err := regexp.Compile(mask)
		if err != nil {
			return nil, fmt.Errorf("redaction mask: %w", err)
		}
		r.masks = append(r.masks, re)
	}
	return r, nil
}

// truncate cuts s to at most maximum bytes without splitting a UTF-8
// sequence. Invalid input is cut at the byte limit.
func truncate(s string, maximum int) string {
	if len(s) <= maximum {
		return s
	}
	cut := maximum
	for cut > 0 && cut > maximum-utf8.UTFMax && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if !utf8.RuneStart(s[cut]) {
		cut = maximum
	}
	return s[:cut]
}

// text masks and truncates s. It returns s itself when nothing changes, so
// callers can skip the write: ReplaceAllString copies its input even without
// a match, and a pdata string value allocates on every SetStr.
func (r *redactor) text(s string, maximum int) string {
	for _, re := range r.masks {
		if re.MatchString(s) {
			s = re.ReplaceAllString(s, "[REDACTED]")
		}
	}
	return truncate(s, maximum)
}

func (r *redactor) attributes(attributes pcommon.Map) {
	attributes.RemoveIf(func(key string, value pcommon.Value) bool {
		if r.deny[key] {
			return true
		}
		r.value(value, r.config.MaxStringLength)
		return false
	})
}

func (r *redactor) value(value pcommon.Value, maximum int) {
	switch value.Type() {
	case pcommon.ValueTypeStr:
		// An unchanged string compares by pointer and is not rewritten.
		s := value.Str()
		if redacted := r.text(s, maximum); redacted != s {
			value.SetStr(redacted)
		}
	case pcommon.ValueTypeBytes:
		if value.Bytes().Len() > maximum {
			value.Bytes().FromRaw(value.Bytes().AsRaw()[:maximum])
		}
	case pcommon.ValueTypeMap:
		r.attributes(value.Map())
	case pcommon.ValueTypeSlice:
		for i := 0; i < value.Slice().Len(); i++ {
			r.value(value.Slice().At(i), maximum)
		}
	}
}

func (r *redactor) resource(resource pcommon.Resource) {
	if r == nil {
		return
	}
	r.attributes(resource.Attributes())
}

func (r *redactor) scope(scope pcommon.InstrumentationScope) {
	if r == nil {
		return
	}
	r.attributes(scope.Attributes())
	scope.SetName(r.text(scope.Name(), r.config.MaxStringLength))
	scope.SetVersion(r.text(scope.Version(), r.config.MaxStringLength))
}

func (r *redactor) span(span ptrace.Span) {
	if r == nil {
		return
	}
	r.attributes(span.Attributes())
	span.SetName(r.text(span.Name(), r.config.MaxStringLength))
	span.Status().SetMessage(r.text(span.Status().Message(), r.config.MaxStringLength))
	for _, event := range span.Events().All() {
		r.attributes(event.Attributes())
		event.SetName(r.text(event.Name(), r.config.MaxStringLength))
	}
	for _, link := range span.Links().All() {
		r.attributes(link.Attributes())
	}
}

func (r *redactor) log(log plog.LogRecord) {
	if r == nil {
		return
	}
	r.attributes(log.Attributes())
	r.value(log.Body(), r.config.MaxLogBodyLength)
	// Bound the complete structured body as well as individual leaf values.
	if log.Body().Type() != pcommon.ValueTypeStr && len(log.Body().AsString()) > r.config.MaxLogBodyLength {
		log.Body().SetStr(truncate("[TRUNCATED]", r.config.MaxLogBodyLength))
	}
	log.SetSeverityText(r.text(log.SeverityText(), r.config.MaxStringLength))
	log.SetEventName(r.text(log.EventName(), r.config.MaxStringLength))
}

// metric redacts the descriptor; datapoints are redacted one by one.
func (r *redactor) metric(metric pmetric.Metric) {
	if r == nil {
		return
	}
	metric.SetName(r.text(metric.Name(), r.config.MaxStringLength))
	metric.SetDescription(r.text(metric.Description(), r.config.MaxStringLength))
	metric.SetUnit(r.text(metric.Unit(), r.config.MaxStringLength))
	r.attributes(metric.Metadata())
}

func (r *redactor) numberDataPoint(point pmetric.NumberDataPoint) {
	if r == nil {
		return
	}
	r.attributes(point.Attributes())
	r.exemplars(point.Exemplars())
}

func (r *redactor) histogramDataPoint(point pmetric.HistogramDataPoint) {
	if r == nil {
		return
	}
	r.attributes(point.Attributes())
	r.exemplars(point.Exemplars())
}

func (r *redactor) exponentialHistogramDataPoint(point pmetric.ExponentialHistogramDataPoint) {
	if r == nil {
		return
	}
	r.attributes(point.Attributes())
	r.exemplars(point.Exemplars())
}

func (r *redactor) summaryDataPoint(point pmetric.SummaryDataPoint) {
	if r == nil {
		return
	}
	r.attributes(point.Attributes())
}

func (r *redactor) exemplars(exemplars pmetric.ExemplarSlice) {
	if r == nil {
		return
	}
	for _, e := range exemplars.All() {
		r.attributes(e.FilteredAttributes())
	}
}
