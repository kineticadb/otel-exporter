// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/kineticadb/kinetica-api-go/kinetica"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

// AttributeValue - struct to contain attribute values of different types
// Used by other metric structs
type AttributeValue struct {
	IntValue    int     `avro:"int_value"`
	StringValue string  `avro:"string_value"`
	BoolValue   int8    `avro:"bool_value"`
	DoubleValue float64 `avro:"double_value"`
	BytesValue  []byte  `avro:"bytes_value"`
}

// NewAttributeValue Constructor for AttributeValue
//
//	@param intValue
//	@param stringValue
//	@param boolValue
//	@param doubleValue
//	@param bytesValue
//	@return *AttributeValue
func NewAttributeValue(intValue int, stringValue string, boolValue int8, doubleValue float64, bytesValue []byte) *AttributeValue {
	o := new(AttributeValue)
	o.IntValue = intValue
	o.StringValue = stringValue
	o.BoolValue = boolValue
	o.DoubleValue = doubleValue
	o.BytesValue = bytesValue
	return o
}

// BEGIN Log Handling

// Log - struct for the Log data
type Log struct {
	LogID                string `mapstructure:"log_id" avro:"log_id"`
	TraceID              string `mapstructure:"trace_id" avro:"trace_id"`
	SpanID               string `mapstructure:"span_id" avro:"span_id"`
	TimeUnixNano         int64  `mapstructure:"time_unix_nano" avro:"time_unix_nano"`
	ObservedTimeUnixNano int64  `mapstructure:"observed_time_unix_nano" avro:"observed_time_unix_nano"`
	SeverityID           int8   `mapstructure:"severity_id" avro:"severity_id"`
	SeverityText         string `mapstructure:"severity_text" avro:"severity_text"`
	Body                 string `mapstructure:"body" avro:"body"`
	Flags                int    `mapstructure:"flags" avro:"flags"`
}

// NewLog Constructor for Logs
//
//	@param logID
//	@param traceID
//	@param spanID
//	@param timeUnixNano
//	@param observedTimeUnixNano
//	@param severityID
//	@param severityText
//	@param body
//	@param flags
//	@return *Log
func NewLog(logID string, traceID string, spanID string, timeUnixNano int64, observedTimeUnixNano int64, severityID int8, severityText string, body string, flags int) *Log {
	o := new(Log)

	o.LogID = logID
	o.TraceID = traceID
	o.SpanID = spanID
	o.TimeUnixNano = timeUnixNano
	o.ObservedTimeUnixNano = observedTimeUnixNano
	o.SeverityID = severityID
	o.SeverityText = severityText
	o.Body = body
	o.Flags = flags

	return o
}

// LogAttribute - struct to contain the Log specific attributes
// contains '[AttributeValue]' struct
type LogAttribute struct {
	LogID          string `avro:"log_id"`
	Key            string `avro:"key"`
	AttributeValue `mapstructure:",squash"`
}

// NewLogAttribute Constructor for LogsAttribute
//
//	@param key
//	@param attributes
//	@return *LogsAttribute
func NewLogAttribute(logID string, key string, attributes AttributeValue) *LogAttribute {
	o := new(LogAttribute)
	o.LogID = logID
	o.Key = key
	o.AttributeValue = attributes
	return o
}

// END LogAttribute

// ResourceAttribute - struct containing the Log Resource attributes
type ResourceAttribute struct {
	LogID          string `avro:"log_id"`
	Key            string `avro:"key"`
	AttributeValue `mapstructure:",squash"`
}

// NewResourceAttribute Constructor for LogsResourceAttribute
//
//	@param resourceID
//	@param key
//	@param attributes
//	@return *LogsResourceAttribute
func NewResourceAttribute(logID string, key string, attributes AttributeValue) *ResourceAttribute {
	o := new(ResourceAttribute)
	o.LogID = logID
	o.Key = key
	o.AttributeValue = attributes
	return o
}

// End LogResourceAttribute

// ScopeAttribute - struct containing the Log Scope attribute
type ScopeAttribute struct {
	LogID          string `avro:"log_id"`
	ScopeName      string `avro:"scope_name"`
	ScopeVersion   string `avro:"scope_version"`
	Key            string `avro:"key"`
	AttributeValue `mapstructure:",squash"`
}

// NewScopeAttribute Constructor for LogsScopeAttribute
//
//	@param scopeID
//	@param key
//	@param scopeName
//	@param scopeVersion
//	@param attributes
//	@return *LogsScopeAttribute
func NewScopeAttribute(logID string, key string, scopeName string, scopeVersion string, attributes AttributeValue) *ScopeAttribute {
	o := new(ScopeAttribute)
	o.LogID = logID
	o.Key = key
	o.ScopeName = scopeName
	o.ScopeVersion = scopeVersion
	o.AttributeValue = attributes
	return o
}

// END LogScopeAttribute

// KiWriter - struct modeling the Kinetica connection, contains the
// Kinetica connection [kinetica.Kinetica], the Kinetica Options [kinetica.KineticaOptions],
// the config [Config] and the logger [zap.Logger]
//
// The connection holds HA, session and fail-back state, so it is shared by
// pointer and never copied. It is nil until [KiWriter.Connect] succeeds.
type KiWriter struct {
	Db      *kinetica.Kinetica
	Options *kinetica.KineticaOptions
	cfg     Config
	logger  *zap.Logger
}

// GetDb - Getter for the Kinetica instance
//
//	@receiver kiwriter
//	@return *kinetica.Kinetica - nil until Connect succeeds
func (kiwriter *KiWriter) GetDb() *kinetica.Kinetica {
	return kiwriter.Db
}

// GetOptions - Getter for the Kinetica options.
//
//	@receiver kiwriter
//	@return *kinetica.KineticaOptions
func (kiwriter *KiWriter) GetOptions() *kinetica.KineticaOptions {
	return kiwriter.Options
}

// GetCfg - Getter for the [Config] value
//
//	@receiver kiwriter
//	@return Config
func (kiwriter *KiWriter) GetCfg() Config {
	return kiwriter.cfg
}

// NewKiWriter - Constructor for the [KiWriter] struct. It does not connect;
// call [KiWriter.Connect] from the exporter's start function.
//
//	@param cfg
//	@param logger
//	@return *KiWriter
func NewKiWriter(cfg Config, logger *zap.Logger) *KiWriter {
	return &KiWriter{Options: newKineticaOptions(&cfg), cfg: cfg, logger: logger}
}

// newKineticaOptions - builds the connection options from the [Config]
//
//	@param cfg
//	@return *kinetica.KineticaOptions
func newKineticaOptions(cfg *Config) *kinetica.KineticaOptions {
	return kinetica.NewKineticaOptions().
		SetUsername(cfg.Username).
		SetPassword(string(cfg.Password)).
		SetByPassSslCertCheck(cfg.BypassSslCertCheck)
}

// Connect - connects to Kinetica, failing if no server answers
//
//	@receiver kiwriter
//	@param ctx - bounds the connection setup only
//	@return error
func (kiwriter *KiWriter) Connect(ctx context.Context) error {
	if kiwriter.Db != nil {
		return nil
	}
	db, err := kinetica.Connect(ctx, []string{kiwriter.cfg.Host}, kiwriter.Options)
	if err != nil {
		return fmt.Errorf("cannot connect to Kinetica at %s: %w", kiwriter.cfg.Host, err)
	}
	kiwriter.Db = db
	return nil
}

// Close - closes the Kinetica connection, if one was made
//
//	@receiver kiwriter
//	@return error
func (kiwriter *KiWriter) Close() error {
	if kiwriter.Db == nil {
		return nil
	}
	err := kiwriter.Db.Close()
	kiwriter.Db = nil
	return err
}

// GetGpuDbInst - Creates and returns a new [kinetica.Kinetica] connection
//
//	@param ctx
//	@param cfg
//	@return *kinetica.Kinetica
//	@return error
func GetGpuDbInst(ctx context.Context, cfg *Config) (*kinetica.Kinetica, error) {
	return kinetica.Connect(ctx, []string{cfg.Host}, newKineticaOptions(cfg))
}

// END Log Handling

// BEGIN Trace Handling

// Span - struct modeling the Span record
type Span struct {
	ID                     string `mapstructure:"id" avro:"id" `
	TraceID                string `mapstructure:"trace_id" avro:"trace_id"`
	SpanID                 string `mapstructure:"span_id" avro:"span_id"`
	ParentSpanID           string `mapstructure:"parent_span_id" avro:"parent_span_id"`
	TraceState             string `mapstructure:"trace_state" avro:"trace_state"`
	Name                   string `mapstructure:"name" avro:"name"`
	SpanKind               int8   `mapstructure:"span_kind" avro:"span_kind"`
	StartTimeUnixNano      int64  `mapstructure:"start_time_unix_nano" avro:"start_time_unix_nano"`
	EndTimeUnixNano        int64  `mapstructure:"end_time_unix_nano" avro:"end_time_unix_nano"`
	DroppedAttributesCount int    `mapstructure:"dropped_attributes_count" avro:"dropped_attributes_count"`
	DroppedEventsCount     int    `mapstructure:"dropped_events_count" avro:"dropped_events_count"`
	DroppedLinksCount      int    `mapstructure:"dropped_links_count" avro:"dropped_links_count"`
	Message                string `mapstructure:"message" avro:"message"`
	StatusCode             int8   `mapstructure:"status_code" avro:"status_code"`
}

// NewSpan Constructor for Span
//
//	@param resourceID
//	@param scopeID
//	@param eventID
//	@param linkID
//	@param traceID
//	@param spanID
//	@param parentSpanID
//	@param traceState
//	@param name
//	@param spanKind
//	@param startTimeUnixNano
//	@param endTimeUnixNano
//	@param droppedAttributeCount
//	@param droppedEventCount
//	@param droppedLinkCount
//	@param message
//	@param statusCode
//	@return *Span
func NewSpan(traceID string, spanID string, parentSpanID string, traceState string, name string, spanKind int8, startTimeUnixNano int64, endTimeUnixNano int64, droppedAttributeCount int, droppedEventCount int, droppedLinkCount int, message string, statusCode int8) *Span {
	o := new(Span)
	o.ID = uuid.New().String()
	o.TraceID = traceID
	o.SpanID = spanID
	o.ParentSpanID = parentSpanID
	o.TraceState = traceState
	o.Name = name
	o.SpanKind = spanKind
	o.StartTimeUnixNano = startTimeUnixNano
	o.EndTimeUnixNano = endTimeUnixNano
	o.DroppedAttributesCount = droppedAttributeCount
	o.DroppedEventsCount = droppedEventCount
	o.DroppedLinksCount = droppedLinkCount
	o.Message = message
	o.StatusCode = statusCode
	return o
}

// SpanAttribute - struct modeling the Trace Span attributes
//
// Contains the [AttributeValue] struct
type SpanAttribute struct {
	SpanID         string `avro:"span_id"`
	Key            string `avro:"key"`
	AttributeValue `mapstructure:",squash"`
}

// NewSpanAttribute Constructor for SpanAttribute
//
//	@param spanID
//	@param key
//	@param atributeValue
//	@return *SpanAttribute
func NewSpanAttribute(spanID string, key string, attributeValue AttributeValue) *SpanAttribute {
	o := new(SpanAttribute)
	o.SpanID = spanID
	o.Key = key
	o.AttributeValue = attributeValue
	return o
}

// TraceResourceAttribute - struct modelig the Trace Resource attributes
type TraceResourceAttribute struct {
	SpanID         string `avro:"span_id"`
	Key            string `avro:"key"`
	AttributeValue `mapstructure:",squash"`
}

// NewTraceResourceAttribute Constructor for TraceResourceAttribute
//
//	@param spanID
//	@param key
//	@param attributes
//	@return *TraceResourceAttribute
func NewTraceResourceAttribute(spanID string, key string, attributes AttributeValue) *TraceResourceAttribute {
	o := new(TraceResourceAttribute)
	o.SpanID = spanID
	o.Key = key
	o.AttributeValue = attributes
	return o
}

// End TraceResourceAttribute

// TraceScopeAttribute - struct modeling the Trace Scope attribute
type TraceScopeAttribute struct {
	SpanID         string `avro:"span_id"`
	ScopeName      string `avro:"scope_name"`
	ScopeVersion   string `avro:"scope_version"`
	Key            string `avro:"key"`
	AttributeValue `mapstructure:",squash"`
}

// NewtraceScopeAttribute Constructor for TraceScopeAttribute
//
//	@param spanID
//	@param key
//	@param scopeName
//	@param scopeVersion
//	@param attributes
//	@return *TraceScopeAttribute
func NewtraceScopeAttribute(spanID string, key string, scopeName string, scopeVersion string, attributes AttributeValue) *TraceScopeAttribute {
	o := new(TraceScopeAttribute)
	o.SpanID = spanID
	o.Key = key
	o.ScopeName = scopeName
	o.ScopeVersion = scopeVersion
	o.AttributeValue = attributes
	return o
}

// END TraceScopeAttribute

// EventAttribute - struct modeling the Trace Event attributes
type EventAttribute struct {
	SpanID         string `avro:"span_id"`
	EventName      string `avro:"event_name"`
	Key            string `avro:"key"`
	AttributeValue `mapstructure:",squash"`
}

// NewEventAttribute Constructor for TraceEventAttribute
//
//	@param key
//	@param eventName
//	@param attributes
//	@return *TraceEventAttribute
func NewEventAttribute(spanID string, eventName string, key string, attributes AttributeValue) *EventAttribute {
	o := new(EventAttribute)
	o.SpanID = spanID
	o.Key = key
	o.EventName = eventName
	o.AttributeValue = attributes
	return o
}

// END TraceEventAttribute

// LinkAttribute - struct modeling the Trace Link attributes
type LinkAttribute struct {
	LinkSpanID     string `avro:"link_span_id"`
	TraceID        string `avro:"trace_id"`
	SpanID         string `avro:"span_id"`
	Key            string `avro:"key"`
	AttributeValue `mapstructure:",squash"`
}

// NewLinkAttribute Constructor for LinkAttribute
//
//	@param linkID
//	@param key
//	@param traceID
//	@param spanID
//	@param attributes
//	@return *LinkAttribute
func NewLinkAttribute(linkSpanID string, key string, traceID string, spanID string, attributes AttributeValue) *LinkAttribute {
	o := new(LinkAttribute)
	o.LinkSpanID = linkSpanID
	o.Key = key
	o.TraceID = traceID
	o.SpanID = spanID
	o.AttributeValue = attributes
	return o
}

// END LinkAttribute

// END Trace Handling

// persistLogRecord - method to write Log records into Kinetica
//
//	@receiver kiwriter
//	@param logRecords
//	@return error
func (kiwriter *KiWriter) persistLogRecord(logRecords []kineticaLogRecord) error {

	var errs []error
	var logs []any
	var logAttribs []interface{}
	var resourceAttribs []interface{}
	var scopeAttribs []interface{}

	for _, logrecord := range logRecords {

		// For each chunk of 10K records persist eveything
		logs = append(logs, *logrecord.log)

		for _, la := range logrecord.logAttribute {
			logAttribs = append(logAttribs, la)
		}

		for _, ra := range logrecord.resourceAttribute {
			resourceAttribs = append(resourceAttribs, ra)
		}

		for _, sa := range logrecord.scopeAttribute {
			scopeAttribs = append(scopeAttribs, sa)
		}
	}

	err := kiwriter.doChunkedInsert(context.TODO(), LogTable, logs)
	if err != nil {
		errs = append(errs, err)
	}

	err = kiwriter.doChunkedInsert(context.TODO(), LogAttributeTable, logAttribs)
	if err != nil {
		errs = append(errs, err)
	}

	err = kiwriter.doChunkedInsert(context.TODO(), LogResourceAttributeTable, resourceAttribs)
	if err != nil {
		errs = append(errs, err)
	}

	err = kiwriter.doChunkedInsert(context.TODO(), LogScopeAttributeTable, scopeAttribs)
	if err != nil {
		errs = append(errs, err)
	}

	return multierr.Combine(errs...)
}

// persistTraceRecord - method to write Trace records into Kinetica
//
//	@receiver kiwriter
//	@param traceRecords
//	@return error
func (kiwriter *KiWriter) persistTraceRecord(traceRecords []kineticaTraceRecord) error {
	var errs []error
	var spans []any
	var spanAttribs []interface{}
	var spanResourceAttribs []interface{}
	var spanScopeAttribs []interface{}
	var spanEventAttribs []interface{}
	var spanLinkAttribs []interface{}

	for _, tracerecord := range traceRecords {

		spans = append(spans, *tracerecord.span)

		for _, spa := range tracerecord.spanAttribute {
			spanAttribs = append(spanAttribs, spa)
		}

		for _, ra := range tracerecord.resourceAttribute {
			spanResourceAttribs = append(spanResourceAttribs, ra)
		}

		for _, sa := range tracerecord.scopeAttribute {
			spanScopeAttribs = append(spanScopeAttribs, sa)
		}

		for _, ea := range tracerecord.eventAttribute {
			spanEventAttribs = append(spanEventAttribs, ea)
		}

		for _, la := range tracerecord.linkAttribute {
			spanLinkAttribs = append(spanLinkAttribs, la)
		}
	}

	err := kiwriter.doChunkedInsert(context.TODO(), TraceSpanTable, spans)
	if err != nil {
		errs = append(errs, err)
	}

	err = kiwriter.doChunkedInsert(context.TODO(), TraceSpanAttributeTable, spanAttribs)
	if err != nil {
		errs = append(errs, err)
	}

	err = kiwriter.doChunkedInsert(context.TODO(), TraceResourceAttributeTable, spanResourceAttribs)
	if err != nil {
		errs = append(errs, err)
	}

	err = kiwriter.doChunkedInsert(context.TODO(), TraceScopeAttributeTable, spanScopeAttribs)
	if err != nil {
		errs = append(errs, err)
	}

	err = kiwriter.doChunkedInsert(context.TODO(), TraceEventAttributeTable, spanEventAttribs)
	if err != nil {
		errs = append(errs, err)
	}

	err = kiwriter.doChunkedInsert(context.TODO(), TraceLinkAttributeTable, spanLinkAttribs)
	if err != nil {
		errs = append(errs, err)
	}

	return multierr.Combine(errs...)
}

func (kiwriter *KiWriter) doChunkedInsert(_ context.Context, tableName string, records []any) error {

	// Build the final table name with the schema prepended
	var finalTable string
	if len(kiwriter.cfg.Schema) != 0 {
		finalTable = fmt.Sprintf("%s.%s", kiwriter.cfg.Schema, tableName)
	} else {
		finalTable = tableName
	}

	kiwriter.logger.Debug("Writing to - ", zap.String("Table", finalTable), zap.Int("Record count", len(records)))

	recordChunks := ChunkBySize(records, ChunkSize)

	errsChan := make(chan error, len(recordChunks))

	wg := &sync.WaitGroup{}

	for _, recordChunk := range recordChunks {
		wg.Add(1)
		go func(data []any, wg *sync.WaitGroup) {
			_, err := kiwriter.Db.InsertRecordsRaw(context.TODO(), finalTable, data)
			errsChan <- err

			wg.Done()
		}(recordChunk, wg)
	}
	wg.Wait()
	close(errsChan)
	var errs error
	for err := range errsChan {
		errs = multierr.Append(errs, err)
	}
	return errs
}
