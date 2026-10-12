// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

func TestCanonicalJSONDoesNotDependOnOrder(t *testing.T) {
	first := pcommon.NewMap()
	first.PutStr("service.name", "checkout")
	first.PutInt("http.response.status_code", 200)
	first.PutBool("retry", true)

	second := pcommon.NewMap()
	second.PutBool("retry", true)
	second.PutInt("http.response.status_code", 200)
	second.PutStr("service.name", "checkout")

	enc := newAttributeEncoder(nil)
	a, err := enc.canonicalJSON(first)
	require.NoError(t, err)
	b, err := enc.canonicalJSON(second)
	require.NoError(t, err)

	assert.Equal(t, a, b)
	assert.Equal(t, `{"http.response.status_code":200,"retry":true,"service.name":"checkout"}`, a)
}

func TestCanonicalJSONKeepsTypes(t *testing.T) {
	attributes := pcommon.NewMap()
	attributes.PutDouble("ratio", 0.25)
	attributes.PutDouble("nan", math.NaN())
	attributes.PutDouble("inf", math.Inf(1))
	attributes.PutEmptyBytes("raw").FromRaw([]byte("hi"))
	attributes.PutEmptySlice("tags").FromRaw([]any{"a", int64(1)})
	attributes.PutEmptyMap("nested").PutStr("k", "<v>")

	text, err := newAttributeEncoder(nil).canonicalJSON(attributes)
	require.NoError(t, err)

	assert.Equal(t, `{"inf":"+Inf","nan":"NaN","nested":{"k":"<v>"},"ratio":0.25,"raw":"aGk=","tags":["a",1]}`, text)
}

func TestLabelsAreStringsWithDottedKeysKept(t *testing.T) {
	attributes := pcommon.NewMap()
	attributes.PutStr("service.name", "checkout")
	attributes.PutInt("http.response.status_code", 503)
	attributes.PutBool("error", true)
	attributes.PutDouble("ratio", 0.5)
	attributes.PutStr("empty", "")
	attributes.PutEmptySlice("tags").FromRaw([]any{"a", int64(2), ""})

	labels := map[string]any{}
	newAttributeEncoder(nil).labels(attributes, labels)
	text, err := marshalJSON(labels)
	require.NoError(t, err)

	assert.Equal(t,
		`{"error":"true","http.response.status_code":"503","ratio":"0.5","service.name":"checkout","tags":["a","2"]}`,
		text)
}

func TestLabelsReplaceEarlierValuesButNotWithEmptyOnes(t *testing.T) {
	labels := map[string]any{"service.name": "from-resource", "host.name": "h1"}

	attributes := pcommon.NewMap()
	attributes.PutStr("service.name", "from-datapoint")
	attributes.PutStr("host.name", "")

	newAttributeEncoder(nil).labels(attributes, labels)

	assert.Equal(t, map[string]any{"service.name": "from-datapoint", "host.name": "h1"}, labels)
}

func TestExcludedKeysAreLeftOut(t *testing.T) {
	attributes := pcommon.NewMap()
	attributes.PutStr("service.name", "checkout")
	attributes.PutStr("user.id", "u-123")

	enc := newAttributeEncoder([]string{"user.id"})
	text, err := enc.canonicalJSON(attributes)
	require.NoError(t, err)
	labels := map[string]any{}
	enc.labels(attributes, labels)

	assert.Equal(t, `{"service.name":"checkout"}`, text)
	assert.Equal(t, map[string]any{"service.name": "checkout"}, labels)
}

func TestSeriesIdentityID(t *testing.T) {
	base := seriesIdentity{
		metricName:         "http.server.requests",
		metricType:         metricTypeSum,
		unit:               "1",
		temporality:        2,
		isMonotonic:        true,
		resourceAttributes: `{"service.name":"checkout"}`,
		scopeName:          "otelhttp",
		scopeVersion:       "1.0",
		scopeAttributes:    `{}`,
		attributes:         `{"http.route":"/cart"}`,
	}
	same := base
	assert.Equal(t, base.id(), same.id())

	changes := map[string]func(s *seriesIdentity){
		"name":        func(s *seriesIdentity) { s.metricName = "http.server.errors" },
		"type":        func(s *seriesIdentity) { s.metricType = metricTypeGauge },
		"unit":        func(s *seriesIdentity) { s.unit = "ms" },
		"temporality": func(s *seriesIdentity) { s.temporality = 1 },
		"monotonic":   func(s *seriesIdentity) { s.isMonotonic = false },
		"resource":    func(s *seriesIdentity) { s.resourceAttributes = `{"service.name":"cart"}` },
		"scope":       func(s *seriesIdentity) { s.scopeName = "other" },
		"attributes":  func(s *seriesIdentity) { s.attributes = `{"http.route":"/pay"}` },
		// The separators keep parts from running together
		"shifted": func(s *seriesIdentity) { s.metricName, s.metricType = "http.server.requestss", "um" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := base
			change(&changed)
			assert.NotEqual(t, base.id(), changed.id())
		})
	}
}
