// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The query and rollup templates shipped with the metric tables
var sqlTemplateFiles = []string{
	filepath.Join("sql", "metrics", "queries.sql"),
	filepath.Join("sql", "metrics", "rollups.sql"),
}

var placeholderPattern = regexp.MustCompile(`\{\{(\w+)\}\}`)

// sqlTemplate - a named statement of a template file
type sqlTemplate struct {
	file        string
	name        string
	description string
	params      []string
	sql         string
}

// render - the statement with its placeholders replaced
func (tpl sqlTemplate) render(values map[string]string) (string, error) {
	var missing []string
	sql := placeholderPattern.ReplaceAllStringFunc(tpl.sql, func(match string) string {
		name := placeholderPattern.FindStringSubmatch(match)[1]
		value, ok := values[name]
		if !ok {
			missing = append(missing, name)
		}
		return value
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%s: no value for %s", tpl.name, strings.Join(missing, ", "))
	}
	return sql, nil
}

// loadSQLTemplates - the templates of a file: each starts with a
// "-- name:" line, followed by comment lines describing it and a
// "-- params:" line, then its statement, ended by a semicolon
func loadSQLTemplates(path string) ([]sqlTemplate, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var templates []sqlTemplate
	var current *sqlTemplate
	var body []string
	finish := func() {
		if current != nil {
			current.sql = strings.TrimSuffix(strings.TrimSpace(strings.Join(body, "\n")), ";")
			templates = append(templates, *current)
		}
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "-- name:"):
			finish()
			current = &sqlTemplate{file: path, name: strings.TrimSpace(strings.TrimPrefix(line, "-- name:"))}
			body = nil
		case current == nil:
			// The file's header
		case strings.HasPrefix(line, "-- params:"):
			for _, param := range strings.Split(strings.TrimPrefix(line, "-- params:"), ",") {
				current.params = append(current.params, strings.TrimSpace(param))
			}
		case strings.HasPrefix(line, "-- ") && len(body) == 0:
			current.description += strings.TrimPrefix(line, "-- ") + " "
		default:
			body = append(body, line)
		}
	}
	finish()
	return templates, scanner.Err()
}

// loadAllSQLTemplates - the templates of every template file
func loadAllSQLTemplates(t *testing.T) []sqlTemplate {
	var templates []sqlTemplate
	for _, path := range sqlTemplateFiles {
		loaded, err := loadSQLTemplates(path)
		require.NoError(t, err)
		require.NotEmpty(t, loaded, path)
		templates = append(templates, loaded...)
	}
	return templates
}

// sampleTemplateValues - values for every placeholder of the templates
func sampleTemplateValues(schema string) map[string]string {
	return map[string]string{
		"schema":         schema,
		"metric":         "http.server.request.duration",
		"label":          "http.request.method",
		"value":          "GET",
		"regex":          "G.*",
		"from":           testTime.Add(-time.Hour).Format(time.DateTime),
		"to":             testTime.Add(time.Hour).Format(time.DateTime),
		"bucket_ms":      "60000",
		"quantile":       "0.95",
		"limit":          "10",
		"retention_days": "3650",
	}
}

func TestSQLTemplates(t *testing.T) {
	names := map[string]string{}
	for _, tpl := range loadAllSQLTemplates(t) {
		t.Run(tpl.name, func(t *testing.T) {
			if other, ok := names[tpl.name]; ok {
				t.Fatalf("%s is also a template of %s", tpl.name, other)
			}
			names[tpl.name] = tpl.file

			assert.NotEmpty(t, strings.TrimSpace(tpl.description), "a template is described")
			assert.NotContains(t, tpl.sql, ";\n\n", "a template holds one statement")

			var used []string
			for _, match := range placeholderPattern.FindAllStringSubmatch(tpl.sql, -1) {
				if !slices.Contains(used, match[1]) {
					used = append(used, match[1])
				}
			}
			assert.ElementsMatch(t, tpl.params, used, "the params line lists the placeholders")

			sql, err := tpl.render(sampleTemplateValues("otel"))
			require.NoError(t, err)
			assert.NotContains(t, sql, "{{")
		})
	}
}

// TestDumpSQL writes every statement the exporter ships to the file named by
// KINETICA_SQL_DUMP, for tools/sqlcheck to parse with Kinetica's SQL parser:
// the table DDL and the templates with sample values. Statements are separated
// by a NUL byte, each starting with a comment naming it.
func TestDumpSQL(t *testing.T) {
	path := os.Getenv("KINETICA_SQL_DUMP")
	if path == "" {
		t.Skip("set KINETICA_SQL_DUMP to a file to write the statements to")
	}
	var statements []string
	add := func(label string, sql string) {
		statements = append(statements, "-- "+label+"\n"+strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	}

	for _, ddl := range metricTableDDLs(defaultLabelsAnalyzer) {
		table := regexp.MustCompile(`CREATE TABLE %s(\w+)`).FindStringSubmatch(ddl)[1]
		add("DDL "+table, strings.ReplaceAll(ddl, "%s", "otel."))
	}
	for _, tpl := range loadAllSQLTemplates(t) {
		sql, err := tpl.render(sampleTemplateValues("otel"))
		require.NoError(t, err)
		add(filepath.Base(tpl.file)+" "+tpl.name, sql)
	}

	require.NoError(t, os.WriteFile(path, []byte(strings.Join(statements, "\x00")), 0o600))
	t.Logf("wrote %d statements to %s", len(statements), path)
}
