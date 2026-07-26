package main

import (
	"testing"

	"memory-vault/internal/store"
)

func TestRowsFromAll(t *testing.T) {
	rows := rowsFromAll([]store.SpaceName{
		{Space: "default", Name: "a", Source: "unspecified", Kind: "note"},
		{Space: "default", Name: "b", Source: "unspecified", Kind: "note"},
		{Space: "work", Name: "c", Source: "claude-code", Kind: "decision"},
	})
	want := []struct {
		isHeader bool
		label    string
	}{
		{true, "default"},
		{false, "a  (unspecified, note)"},
		{false, "b  (unspecified, note)"},
		{true, "work"},
		{false, "c  (claude-code, decision)"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rowsFromAll: got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].isHeader != w.isHeader || rows[i].label != w.label {
			t.Errorf("rows[%d] = %+v, want isHeader=%v label=%q", i, rows[i], w.isHeader, w.label)
		}
	}
}

func TestMoveCursorSkipsHeaders(t *testing.T) {
	m := model{rows: rowsFromAll([]store.SpaceName{
		{Space: "default", Name: "a"},
		{Space: "work", Name: "b"},
	})}
	// rows: [header default, a, header work, b]
	m.cursor = 1 // "a"
	m.moveCursor(1)
	if m.cursor != 3 || m.rows[m.cursor].name != "b" {
		t.Errorf("moveCursor(1) landed on row %d (%+v), want row 3 (b)", m.cursor, m.rows[m.cursor])
	}
}
