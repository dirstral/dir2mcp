// Package lifecycle holds a small, dependency-free transition table for the
// string-valued state columns dir2mcp persists (documents.status,
// chunks.embedding_status).
//
// A Table only describes a lifecycle. It does not enforce one: callers use it to
// validate values, to RECORD a transition the table does not list, and to
// generate docs/state-machines.md. Refusing a transition is a separate,
// deliberate change.
package lifecycle

import (
	"sort"
)

// Edge is one transition, in string form so it can key maps and render docs
// without knowing the concrete state type.
type Edge struct {
	From string
	To   string
}

// Table is the lifecycle of one entity whose state is a typed string S.
type Table[S ~string] struct {
	// Name names the entity and its column, for docs and log lines.
	Name string
	// States lists every value the column can hold, in declaration order.
	States []S
	// Initial lists the states a NEW row can be written in.
	Initial []S
	// Allowed maps a state to the states a later write can move it to.
	Allowed map[S][]S
	// Via optionally names the code path behind an edge, for the docs. An
	// edge with From == "" is an initial write.
	Via map[Edge]string
}

// Valid reports whether s is one of the table's states.
func (t Table[S]) Valid(s S) bool {
	for _, known := range t.States {
		if known == s {
			return true
		}
	}
	return false
}

// CanInitial reports whether a new row can be written in state s.
func (t Table[S]) CanInitial(s S) bool {
	for _, known := range t.Initial {
		if known == s {
			return true
		}
	}
	return false
}

// CanTransition reports whether the table lists the edge from -> to.
func (t Table[S]) CanTransition(from, to S) bool {
	for _, next := range t.Allowed[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Transitions returns every listed edge, initial writes first (From == ""),
// then sorted by the declaration order of From and To in States.
func (t Table[S]) Transitions() []Edge {
	rank := make(map[string]int, len(t.States))
	for i, s := range t.States {
		rank[string(s)] = i + 1
	}
	out := make([]Edge, 0, len(t.Initial)+len(t.Allowed)*len(t.States))
	for _, s := range t.Initial {
		out = append(out, Edge{From: "", To: string(s)})
	}
	for from, tos := range t.Allowed {
		for _, to := range tos {
			out = append(out, Edge{From: string(from), To: string(to)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].From] != rank[out[j].From] {
			return rank[out[i].From] < rank[out[j].From]
		}
		return rank[out[i].To] < rank[out[j].To]
	})
	return out
}

// Observe checks one write against the table and calls logf once when the
// write is not listed. It never refuses the write. from == "" means a new row.
// It returns true when the write is listed. A nil logf only reports.
func (t Table[S]) Observe(logf func(format string, args ...any), id string, from, to S) bool {
	listed := t.CanTransition(from, to)
	if from == "" {
		listed = t.CanInitial(to)
	}
	if !listed && logf != nil {
		shown := string(from)
		if shown == "" {
			shown = "(new)"
		}
		logf("lifecycle: unlisted transition %s %s %s->%s", t.Name, id, shown, string(to))
	}
	return listed
}

// MarkdownTable renders the table's edges as a Markdown table with a From, To
// and Via column. It is the shared body of docs/state-machines.md.
func (t Table[S]) MarkdownTable() string {
	var b []byte
	b = append(b, "| From | To | Via |\n| --- | --- | --- |\n"...)
	for _, e := range t.Transitions() {
		from := "`" + e.From + "`"
		if e.From == "" {
			from = "(new row)"
		}
		via := t.Via[e]
		if via == "" {
			via = "-"
		}
		b = append(b, "| "+from+" | `"+e.To+"` | "+via+" |\n"...)
	}
	return string(b)
}
