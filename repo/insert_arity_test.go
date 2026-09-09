package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// insertHeadPattern matches the "INSERT INTO <table> (<columns>) VALUES (" prefix. The
// VALUES list itself is read by balancing parentheses, not by regex, so an expression
// like NOW() does not truncate it.
var insertHeadPattern = regexp.MustCompile(`(?is)INSERT\s+INTO\s+([a-zA-Z_][\w.]*)\s*\(([^)]*)\)\s*VALUES\s*\(`)

// TestInsertStatementsHaveMatchingArity guards a failure this package's other tests are
// structurally blind to: go-sqlmock never parses the SQL it is handed. It regex-matches
// the query text and compares the argument list, so an INSERT whose VALUES list carries
// one more placeholder than the column list passes every unit test and then fails in
// production with "INSERT has more expressions than target columns".
//
// That is not hypothetical. Dropping doku_subaccount_id in the Singapay migration removed
// a column and its argument but left VALUES at $14 against 13 columns, and the first
// seller to reach a paid booking hit it — after their Singapay sub-account had already
// been created, which is the expensive half of CreateAccount to have to undo by hand.
//
// Editing a column list without editing the placeholder list is the natural way to make
// this mistake, so the check reads the statements as text rather than exercising them.
func TestInsertStatementsHaveMatchingArity(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		source, err := os.ReadFile(filepath.Clean(name))
		require.NoError(t, err)
		src := string(source)

		for _, match := range insertHeadPattern.FindAllStringSubmatchIndex(src, -1) {
			table := src[match[2]:match[3]]
			columns := splitTopLevel(src[match[4]:match[5]])

			values, ok := readParenthesized(src, match[1]-1)
			require.Truef(t, ok, "%s: unbalanced VALUES list for %s", name, table)
			expressions := splitTopLevel(values)

			line := strings.Count(src[:match[0]], "\n") + 1
			require.Lenf(t, expressions, len(columns),
				"%s:%d: INSERT INTO %s lists %d columns but %d VALUES expressions",
				name, line, table, len(columns), len(expressions))
			checked++
		}
	}

	// A refactor that moved every statement out of this package would otherwise leave the
	// test passing while checking nothing.
	require.NotZero(t, checked, "no INSERT statements found to check")
}

// readParenthesized returns the contents of the parenthesized group opening at src[open].
func readParenthesized(src string, open int) (string, bool) {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return src[open+1 : i], true
			}
		}
	}
	return "", false
}

// splitTopLevel splits a comma-separated list, ignoring commas nested inside parentheses
// so that a call such as COALESCE(a, b) counts as the single expression it is.
func splitTopLevel(list string) []string {
	var parts []string
	var current strings.Builder
	depth := 0

	flush := func() {
		if part := strings.TrimSpace(current.String()); part != "" {
			parts = append(parts, part)
		}
		current.Reset()
	}

	for i := 0; i < len(list); i++ {
		switch c := list[i]; c {
		case '(':
			depth++
			current.WriteByte(c)
		case ')':
			depth--
			current.WriteByte(c)
		case ',':
			if depth == 0 {
				flush()
			} else {
				current.WriteByte(c)
			}
		default:
			current.WriteByte(c)
		}
	}
	flush()

	return parts
}
