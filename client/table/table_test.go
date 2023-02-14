// Forked from https://github.com/charmbracelet/bubbles/blob/master/table/table_test.go to add horizontal scrolling

package table

import (
	"testing"

	"github.com/ddworken/hishtory/shared/testutils"
)

func TestFromValues(t *testing.T) {
	input := "foo1,bar1\nfoo2,bar2\nfoo3,bar3"
	table := New(WithColumns([]Column{{Title: "Foo"}, {Title: "Bar"}}))
	table.FromValues(input, ",")

	if len(table.rows) != 3 {
		t.Fatalf("expect table to have 3 rows but it has %d", len(table.rows))
	}

	expect := []Row{
		{"foo1", "bar1"},
		{"foo2", "bar2"},
		{"foo3", "bar3"},
	}
	if !deepEqual(table.rows, expect) {
		t.Fatal("table rows is not equals to the input")
	}
}

func TestFromValuesWithTabSeparator(t *testing.T) {
	input := "foo1.\tbar1\nfoo,bar,baz\tbar,2"
	table := New(WithColumns([]Column{{Title: "Foo"}, {Title: "Bar"}}))
	table.FromValues(input, "\t")

	if len(table.rows) != 2 {
		t.Fatalf("expect table to have 2 rows but it has %d", len(table.rows))
	}

	expect := []Row{
		{"foo1.", "bar1"},
		{"foo,bar,baz", "bar,2"},
	}
	if !deepEqual(table.rows, expect) {
		t.Fatal("table rows is not equals to the input")
	}
}

func TestHScoll(t *testing.T) {
	table := New(
		WithColumns([]Column{{Title: "Column1", Width: 10}, {Title: "Column2", Width: 20}}),
		WithRows([]Row{
			{"a1", "a2345"},
			{"b1", "b23"},
			{"c1", "c1234567890abcdefghijklmnopqrstuvwxyz"},
		}),
	)
	testutils.CompareGoldens(t, table.View(), "unittestTable-truncatedTable")
	table.MoveRight(1)
	testutils.CompareGoldens(t, table.View(), "unittestTable-truncatedTable-right1")
	table.MoveRight(1)
	testutils.CompareGoldens(t, table.View(), "unittestTable-truncatedTable-right2")
	table.MoveRight(1)
	testutils.CompareGoldens(t, table.View(), "unittestTable-truncatedTable-right3")
	table.MoveLeft(1)
	testutils.CompareGoldens(t, table.View(), "unittestTable-truncatedTable-right2")
}

func deepEqual(a, b []Row) bool {
	if len(a) != len(b) {
		return false
	}
	for i, r := range a {
		for j, f := range r {
			if f != b[i][j] {
				return false
			}
		}
	}
	return true
}
