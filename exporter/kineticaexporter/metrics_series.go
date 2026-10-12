// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"

	"github.com/cespare/xxhash/v2"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

// Label keys the instrumentation scope adds to a series' labels
const (
	scopeNameLabel    = "otel.scope.name"
	scopeVersionLabel = "otel.scope.version"
	serviceNameKey    = "service.name"
)

// attributeEncoder - encodes attribute maps for the metric tables
type attributeEncoder struct {
	exclude map[string]struct{}
}

// newAttributeEncoder - constructor for [attributeEncoder]
//
//	@param excludeKeys - attribute keys left out of every encoding
//	@return *attributeEncoder
func newAttributeEncoder(excludeKeys []string) *attributeEncoder {
	exclude := make(map[string]struct{}, len(excludeKeys))
	for _, key := range excludeKeys {
		exclude[key] = struct{}{}
	}
	return &attributeEncoder{exclude: exclude}
}

// excluded - whether a key is left out
func (enc *attributeEncoder) excluded(key string) bool {
	_, ok := enc.exclude[key]
	return ok
}

// canonicalJSON - the attributes as a JSON object keeping their types, with
// the keys sorted, so equal maps give equal text whatever their order. It is
// both the fidelity copy stored for a series and its part of the identity.
//
//	@receiver enc
//	@param attributes
//	@return string
//	@return error
func (enc *attributeEncoder) canonicalJSON(attributes pcommon.Map) (string, error) {
	object := make(map[string]any, attributes.Len())
	attributes.Range(func(key string, value pcommon.Value) bool {
		if !enc.excluded(key) {
			object[key] = typedJSONValue(value)
		}
		return true
	})
	return marshalJSON(object)
}

// labels - the attributes as label values: every scalar as a string, so a
// label matches the same way whatever its type, as Prometheus labels do, and
// empty values left out, as Prometheus leaves them out. Keys are kept as they
// are; the text index reads a dotted key as a path.
//
//	@receiver enc
//	@param attributes
//	@param into - the labels to add to; a key already there is replaced
func (enc *attributeEncoder) labels(attributes pcommon.Map, into map[string]any) {
	attributes.Range(func(key string, value pcommon.Value) bool {
		if enc.excluded(key) {
			return true
		}
		if label, ok := labelValue(value); ok {
			into[key] = label
		}
		return true
	})
}

// typedJSONValue - an attribute value as JSON keeps it: numbers as numbers,
// bytes as base64, and the floating point values JSON cannot hold (NaN and
// the infinities) as their names
//
//	@param value
//	@return any
func typedJSONValue(value pcommon.Value) any {
	switch value.Type() {
	case pcommon.ValueTypeStr:
		return value.Str()
	case pcommon.ValueTypeBool:
		return value.Bool()
	case pcommon.ValueTypeInt:
		return value.Int()
	case pcommon.ValueTypeDouble:
		return jsonFloat(value.Double())
	case pcommon.ValueTypeBytes:
		return base64.StdEncoding.EncodeToString(value.Bytes().AsRaw())
	case pcommon.ValueTypeSlice:
		slice := value.Slice()
		items := make([]any, slice.Len())
		for i := 0; i < slice.Len(); i++ {
			items[i] = typedJSONValue(slice.At(i))
		}
		return items
	case pcommon.ValueTypeMap:
		object := make(map[string]any, value.Map().Len())
		value.Map().Range(func(key string, v pcommon.Value) bool {
			object[key] = typedJSONValue(v)
			return true
		})
		return object
	default:
		return nil
	}
}

// labelValue - an attribute value as a label: scalars as strings, a slice as
// an array of labels and a map as an object of them
//
//	@param value
//	@return any
//	@return bool - false for an empty value, which is left out
func labelValue(value pcommon.Value) (any, bool) {
	switch value.Type() {
	case pcommon.ValueTypeStr:
		return value.Str(), value.Str() != ""
	case pcommon.ValueTypeBool:
		return strconv.FormatBool(value.Bool()), true
	case pcommon.ValueTypeInt:
		return strconv.FormatInt(value.Int(), 10), true
	case pcommon.ValueTypeDouble:
		return strconv.FormatFloat(value.Double(), 'g', -1, 64), true
	case pcommon.ValueTypeBytes:
		raw := value.Bytes().AsRaw()
		return base64.StdEncoding.EncodeToString(raw), len(raw) > 0
	case pcommon.ValueTypeSlice:
		slice := value.Slice()
		items := make([]any, 0, slice.Len())
		for i := 0; i < slice.Len(); i++ {
			if item, ok := labelValue(slice.At(i)); ok {
				items = append(items, item)
			}
		}
		return items, len(items) > 0
	case pcommon.ValueTypeMap:
		object := make(map[string]any, value.Map().Len())
		value.Map().Range(func(key string, v pcommon.Value) bool {
			if label, ok := labelValue(v); ok {
				object[key] = label
			}
			return true
		})
		return object, len(object) > 0
	default:
		return nil, false
	}
}

// jsonFloat - a float as JSON can hold it
func jsonFloat(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return f
}

// marshalJSON - JSON text with sorted keys and without HTML escaping
//
//	@param value
//	@return string
//	@return error
func marshalJSON(value any) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// seriesIdentity - what makes a time series distinct. The attribute fields
// hold canonical JSON, so the identity does not depend on attribute order.
type seriesIdentity struct {
	metricName         string
	metricType         string
	unit               string
	temporality        int8
	isMonotonic        bool
	resourceAttributes string
	scopeName          string
	scopeVersion       string
	scopeAttributes    string
	attributes         string
}

// id - the 64-bit xxhash of the identity. The parts are separated by a zero
// byte, which none of them holds (JSON escapes it), so different identities
// cannot run together into the same bytes.
//
//	@receiver s
//	@return int64
func (s *seriesIdentity) id() int64 {
	h := xxhash.New()
	for _, part := range []string{
		s.metricName,
		s.metricType,
		s.unit,
		strconv.Itoa(int(s.temporality)),
		strconv.FormatBool(s.isMonotonic),
		s.resourceAttributes,
		s.scopeName,
		s.scopeVersion,
		s.scopeAttributes,
		s.attributes,
	} {
		_, _ = h.WriteString(part)
		_, _ = h.Write([]byte{0})
	}
	return int64(h.Sum64())
}
