package postgres

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func tracer() oteltrace.Tracer {
	return otel.Tracer("github.com/aszender/payflow/internal/repository/postgres")
}

func recordSpanError(span oteltrace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
