// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Paths;

import org.apache.calcite.avatica.util.Casing;
import org.apache.calcite.parser.KineticaParserImpl;
import org.apache.calcite.sql.parser.SqlParser;

import static org.apache.calcite.sql.validate.SqlConformanceEnum.LENIENT;

/**
 * Parses SQL statements with Kinetica's SQL parser, configured as the
 * server's query planner configures it (KineticaQueryPlanner). It checks
 * syntax only: names, types and functions are not resolved.
 *
 * Each argument is a file of statements separated by NUL bytes, each
 * statement's first line naming it. Prints OK or FAIL for each statement and
 * exits with 1 if any failed.
 */
public class KiParse {
    public static void main(String[] args) throws Exception {
        @SuppressWarnings("deprecation")
        SqlParser.Config config = SqlParser.configBuilder()
            .setConformance(LENIENT)
            .setParserFactory(KineticaParserImpl.FACTORY)
            .setQuotedCasing(Casing.UNCHANGED)
            .setUnquotedCasing(Casing.UNCHANGED)
            .setCaseSensitive(false)
            .setIdentifierMaxLength(200)
            .build();

        int total = 0;
        int failed = 0;
        for (String file : args) {
            String text = new String(Files.readAllBytes(Paths.get(file)), StandardCharsets.UTF_8);
            for (String statement : text.split("\u0000")) {
                String sql = statement.trim();
                if (sql.isEmpty()) {
                    continue;
                }
                total++;
                String label = sql.lines().findFirst().orElse("");
                try {
                    SqlParser.create(sql, config).parseStmt();
                    System.out.println("OK    " + label);
                } catch (Exception e) {
                    failed++;
                    String message = e.getMessage() == null ? e.toString() : e.getMessage();
                    System.out.println("FAIL  " + label + "\n      " + message.split("\n")[0]);
                }
            }
        }
        System.out.println(total + " statement(s), " + failed + " failed");
        System.exit(failed == 0 ? 0 : 1);
    }
}
