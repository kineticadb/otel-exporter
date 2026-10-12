// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	sqlCommentPattern     = regexp.MustCompile(`--[^\n]*`)
	sqlSpacePattern       = regexp.MustCompile(`\s+`)
	sqlPunctuationPattern = regexp.MustCompile(`\s*([(),])\s*`)
	createTablePattern    = regexp.MustCompile(`^create table (?:otel\.)?(\w+)`)
)

// normalizedTables - the CREATE TABLE statements of SQL text, by table name,
// with comments, double quotes, case and spacing taken out so that only
// their meaning is compared
func normalizedTables(t *testing.T, sql string) map[string]string {
	t.Helper()
	tables := map[string]string{}
	for _, statement := range strings.Split(sqlCommentPattern.ReplaceAllString(sql, ""), ";") {
		normalized := strings.ToLower(strings.ReplaceAll(statement, `"`, ""))
		normalized = sqlSpacePattern.ReplaceAllString(normalized, " ")
		normalized = strings.TrimSpace(sqlPunctuationPattern.ReplaceAllString(normalized, "$1"))
		if normalized == "" {
			continue
		}
		match := createTablePattern.FindStringSubmatch(normalized)
		require.NotNil(t, match, "not a CREATE TABLE statement: %s", normalized)
		require.NotContains(t, tables, match[1], "%s is created twice", match[1])
		tables[match[1]] = normalized
	}
	return tables
}

// TestDatamodelSQL checks that sql/datamodel.sql holds the statements the
// exporter runs when it starts, in the schema otel: logs, traces and
// metrics, the metrics with the default labels analyzer.
func TestDatamodelSQL(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("sql", "datamodel.sql"))
	require.NoError(t, err)
	documented := normalizedTables(t, string(text))

	var ddls []string
	for _, list := range [][]string{
		logTableDDLs, traceTableDDLs, metricTableDDLs(defaultLabelsAnalyzer),
	} {
		ddls = append(ddls, list...)
	}
	created := map[string]string{}
	for _, ddl := range ddls {
		for table, statement := range normalizedTables(t, strings.ReplaceAll(ddl, "%s", "otel.")) {
			created[table] = statement
		}
	}

	documentedNames := make([]string, 0, len(documented))
	for table := range documented {
		documentedNames = append(documentedNames, table)
	}
	createdNames := make([]string, 0, len(created))
	for table := range created {
		createdNames = append(createdNames, table)
	}
	slices.Sort(documentedNames)
	slices.Sort(createdNames)
	require.Equal(t, createdNames, documentedNames, "datamodel.sql has the tables the exporter creates")

	for _, table := range createdNames {
		assert.Equal(t, created[table], documented[table], "the statement creating %s", table)
	}
}
