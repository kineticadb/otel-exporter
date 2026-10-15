#!/bin/sh
# Copyright The OpenTelemetry Authors
# SPDX-License-Identifier: Apache-2.0
#
# Checks the syntax of every SQL statement the exporter ships, with Kinetica's
# own SQL parser: the metric table DDL and the query and rollup templates of
# sql/metrics. No server is needed, only the gpudbsql jar of a Kinetica
# install, a JDK and Go.
#
# Usage:
#   KINETICA_SQL_JAR=/opt/gpudb/sql/gpudbsql-7.2.3.0.jar tools/sqlcheck/check.sh
#
# From a Kinetica container:
#   docker cp <container>:/opt/gpudb/sql/gpudbsql-7.2.3.0.jar /tmp/
set -eu

: "${KINETICA_SQL_JAR:?set KINETICA_SQL_JAR to the gpudbsql jar of a Kinetica install, e.g. /opt/gpudb/sql/gpudbsql-7.2.3.0.jar}"

here=$(cd "$(dirname "$0")" && pwd)
package=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

javac -proc:none -nowarn -cp "$KINETICA_SQL_JAR" -d "$work" "$here/KiParse.java"
(cd "$package" && KINETICA_SQL_DUMP="$work/statements.sql" go test -count=1 -run '^TestDumpSQL$' . > /dev/null)

status=0
java -Dlog.file.name="$work/parser.log" -cp "$KINETICA_SQL_JAR:$work" KiParse "$work/statements.sql" > "$work/result.txt" 2>&1 || status=$?
grep -v -i -e 'slf4j' -e 'log4j' "$work/result.txt"
exit $status
