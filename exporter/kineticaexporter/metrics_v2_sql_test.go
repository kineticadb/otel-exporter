// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"bufio"
	"encoding/json"
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

// The SQL shipped with the v2 metrics layout: query and rollup templates, and
// the queries of the Grafana dashboards
var (
	sqlTemplateFiles = []string{
		filepath.Join("sql", "metrics_v2", "queries.sql"),
		filepath.Join("sql", "metrics_v2", "rollups.sql"),
	}
	dashboardGlob = filepath.Join("grafana", "dashboards", "*.json")
)

var (
	placeholderPattern = regexp.MustCompile(`\{\{(\w+)\}\}`)
	// The variables and macros a panel query may hold: the plugin expands
	// these two macros and nothing else, not even dashboard variables
	panelVariablePattern = regexp.MustCompile(`\$\{?\w+`)
	pluginMacros         = []string{"$__timeFrom", "$__timeTo"}
	// How the plugin finds the table whose TIMESTAMP columns it marks as
	// time: the first name after a FROM (pkg/plugin/datasource.go)
	pluginFromTablePattern = regexp.MustCompile(`(?i)\bFROM\s+([a-zA-Z0-9_."]+)`)
)

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

// grafanaDashboard - the parts of a dashboard the tests check
type grafanaDashboard struct {
	UID        string `json:"uid"`
	Title      string `json:"title"`
	Templating struct {
		List []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Query string `json:"query"`
		} `json:"list"`
	} `json:"templating"`
	Panels []struct {
		ID         int    `json:"id"`
		Type       string `json:"type"`
		Title      string `json:"title"`
		Datasource *struct {
			Type string `json:"type"`
			UID  string `json:"uid"`
		} `json:"datasource"`
		Targets []struct {
			RefID  string `json:"refId"`
			RawSQL string `json:"rawSql"`
		} `json:"targets"`
		Transformations []struct {
			ID      string `json:"id"`
			Options struct {
				Format      string `json:"format"`
				Conversions []struct {
					TargetField     string `json:"targetField"`
					DestinationType string `json:"destinationType"`
				} `json:"conversions"`
			} `json:"options"`
		} `json:"transformations"`
	} `json:"panels"`
}

// loadDashboards - the dashboards, by file name
func loadDashboards(t *testing.T) map[string]grafanaDashboard {
	paths, err := filepath.Glob(dashboardGlob)
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	dashboards := map[string]grafanaDashboard{}
	for _, path := range paths {
		text, err := os.ReadFile(path)
		require.NoError(t, err)
		var dashboard grafanaDashboard
		require.NoError(t, json.Unmarshal(text, &dashboard), path)
		dashboards[filepath.Base(path)] = dashboard
	}
	return dashboards
}

// expandPluginMacros - a panel query as the Kinetica Grafana plugin sends it
// to the server
func expandPluginMacros(sql string, from time.Time, to time.Time) string {
	const layout = "2006-01-02 15:04:05.000"
	sql = strings.ReplaceAll(sql, "$__timeFrom()", "'"+from.UTC().Format(layout)+"'")
	return strings.ReplaceAll(sql, "$__timeTo()", "'"+to.UTC().Format(layout)+"'")
}

// withSchema - a dashboard query reading another schema than otel
func withSchema(sql string, schema string) string {
	return regexp.MustCompile(`(?i)\b(FROM|JOIN)(\s+)otel\.`).ReplaceAllString(sql, "${1}${2}"+schema+".")
}

// timestampColumns - the TIMESTAMP columns of each v2 table
func timestampColumns() map[string][]string {
	tablePattern := regexp.MustCompile(`CREATE TABLE %s(\w+)`)
	columnPattern := regexp.MustCompile(`(?m)^\s*(\w+) TIMESTAMP`)
	columns := map[string][]string{}
	for _, ddl := range metricsV2TableDDLs("") {
		table := tablePattern.FindStringSubmatch(ddl)[1]
		for _, match := range columnPattern.FindAllStringSubmatch(ddl, -1) {
			columns[table] = append(columns[table], match[1])
		}
	}
	return columns
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

func TestDashboards(t *testing.T) {
	uids := map[string]string{}
	timeColumns := timestampColumns()

	for file, dashboard := range loadDashboards(t) {
		t.Run(file, func(t *testing.T) {
			require.NotEmpty(t, dashboard.UID)
			assert.NotContains(t, uids, dashboard.UID, "dashboard uids are unique")
			uids[dashboard.UID] = file

			require.Len(t, dashboard.Templating.List, 1, "the datasource is the only variable")
			variable := dashboard.Templating.List[0]
			assert.Equal(t, "DS_KINETICA", variable.Name)
			assert.Equal(t, "datasource", variable.Type)
			assert.Equal(t, "kinetica-datasource", variable.Query)

			for _, panel := range dashboard.Panels {
				if panel.Type == "text" {
					continue
				}
				t.Run(panel.Title, func(t *testing.T) {
					require.NotNil(t, panel.Datasource)
					assert.Equal(t, "kinetica-datasource", panel.Datasource.Type)
					assert.Equal(t, "${DS_KINETICA}", panel.Datasource.UID)
					require.Len(t, panel.Targets, 1)
					sql := panel.Targets[0].RawSQL

					for _, variable := range panelVariablePattern.FindAllString(sql, -1) {
						assert.Contains(t, pluginMacros, variable, "the plugin does not expand dashboard variables")
					}
					assert.Regexp(t, `\b(FROM|JOIN)\s+otel\.`, sql, "queries read the otel schema")

					if panel.Type != "timeseries" {
						return
					}
					// The plugin marks a column as time only when the first
					// table after FROM has a TIMESTAMP column of its name
					require.Len(t, panel.Transformations, 2)
					convert, prepare := panel.Transformations[0], panel.Transformations[1]
					assert.Equal(t, "convertFieldType", convert.ID)
					require.Len(t, convert.Options.Conversions, 1)
					timeColumn := convert.Options.Conversions[0].TargetField
					assert.Equal(t, "time", convert.Options.Conversions[0].DestinationType)
					assert.Equal(t, "prepareTimeSeries", prepare.ID)
					assert.Equal(t, "multi", prepare.Options.Format)

					match := pluginFromTablePattern.FindStringSubmatch(sql)
					require.NotNil(t, match)
					table := strings.TrimPrefix(match[1], "otel.")
					assert.Contains(t, timeColumns[table], timeColumn,
						"the first table after FROM, %s, has the TIMESTAMP column %s", table, timeColumn)
					assert.Contains(t, sql, " AS "+timeColumn+",", "the query names its time column")
				})
			}
		})
	}
}

func TestExpandPluginMacros(t *testing.T) {
	sql := expandPluginMacros("WHERE t BETWEEN $__timeFrom() AND $__timeTo()", testTime, testTime.Add(time.Hour))

	assert.Equal(t, "WHERE t BETWEEN '2026-10-12 10:00:00.000' AND '2026-10-12 11:00:00.000'", sql)
}

func TestWithSchema(t *testing.T) {
	sql := withSchema(`SELECT JSON_VALUE(labels, '$["otel.scope.name"]') FROM otel.metric_series s
JOIN otel.metric_number_point p ON p.series_id = s.series_id`, "it")

	assert.Equal(t, `SELECT JSON_VALUE(labels, '$["otel.scope.name"]') FROM it.metric_series s
JOIN it.metric_number_point p ON p.series_id = s.series_id`, sql)
}

// TestDumpSQL writes every statement the exporter ships to the file named by
// KINETICA_SQL_DUMP, for tools/sqlcheck to parse with Kinetica's SQL parser:
// the table DDL, the templates with sample values and the dashboard queries
// with the plugin's macros expanded. Statements are separated by a NUL byte,
// each starting with a comment naming it.
func TestDumpSQL(t *testing.T) {
	path := os.Getenv("KINETICA_SQL_DUMP")
	if path == "" {
		t.Skip("set KINETICA_SQL_DUMP to a file to write the statements to")
	}
	var statements []string
	add := func(label string, sql string) {
		statements = append(statements, "-- "+label+"\n"+strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	}

	for _, ddl := range metricsV2TableDDLs(defaultLabelsAnalyzer) {
		table := regexp.MustCompile(`CREATE TABLE %s(\w+)`).FindStringSubmatch(ddl)[1]
		add("DDL "+table, strings.ReplaceAll(ddl, "%s", "otel."))
	}
	for _, tpl := range loadAllSQLTemplates(t) {
		sql, err := tpl.render(sampleTemplateValues("otel"))
		require.NoError(t, err)
		add(filepath.Base(tpl.file)+" "+tpl.name, sql)
	}
	dashboards := loadDashboards(t)
	files := make([]string, 0, len(dashboards))
	for file := range dashboards {
		files = append(files, file)
	}
	slices.Sort(files)
	for _, file := range files {
		for _, panel := range dashboards[file].Panels {
			for _, target := range panel.Targets {
				add(file+" "+panel.Title, expandPluginMacros(target.RawSQL, testTime.Add(-time.Hour), testTime))
			}
		}
	}

	require.NoError(t, os.WriteFile(path, []byte(strings.Join(statements, "\x00")), 0o600))
	t.Logf("wrote %d statements to %s", len(statements), path)
}
