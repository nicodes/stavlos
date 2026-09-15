package agent

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/toolname"
	"github.com/nicodes/stavlos/internal/tools"
)

// The todo list is state in the log: every change is a todo.changed event
// carrying the whole list, applied like any other, so recovery restores it
// and every client renders the same list.

// todoAPIFor is the list for a tool env, nil when the role does not
// include "todo" (the tool then says it is unavailable).
func (a *Agent) todoAPIFor(rv roleView) tools.Todos {
	if contains(rv.preset.Tools, toolname.Todo) {
		return todosAPI{a: a}
	}
	return nil
}

type todosAPI struct{ a *Agent }

func (t todosAPI) Edit(add []string, updates []tools.TodoUpdate) ([]event.TodoItem, error) {
	var out []event.TodoItem
	err := t.change(func(st *agentState, items []event.TodoItem) ([]event.TodoItem, error) {
		for _, u := range updates {
			i := slices.IndexFunc(items, func(it event.TodoItem) bool { return it.ID == u.ID })
			if i < 0 {
				return nil, fmt.Errorf("no todo item %q", u.ID)
			}
			if u.Status != "" {
				items[i].Status = event.TodoStatus(u.Status)
			}
			if u.Text != "" {
				items[i].Text = u.Text
			}
		}
		next := st.todoSeq // ids keep counting past every item there has been
		for _, it := range items {
			if n, err := strconv.Atoi(strings.TrimPrefix(it.ID, "t")); err == nil {
				next = max(next, n)
			}
		}
		for _, text := range add {
			next++
			items = append(items, event.TodoItem{ID: "t" + strconv.Itoa(next), Text: text, Status: event.TodoPending})
		}
		out = items
		return items, nil
	})
	return out, err
}

func (t todosAPI) List() []event.TodoItem {
	t.a.c.mu.Lock()
	defer t.a.c.mu.Unlock()
	return append([]event.TodoItem(nil), t.a.state().todos...)
}

// change logs the list edit makes of a copy of the current one.
func (t todosAPI) change(edit func(*agentState, []event.TodoItem) ([]event.TodoItem, error)) error {
	s := t.a.c
	s.mu.Lock()
	defer s.mu.Unlock()
	st := t.a.state()
	items, err := edit(st, append([]event.TodoItem(nil), st.todos...))
	if err != nil {
		return err
	}
	_, err = s.commitLocked(context.Background(), s.event(t.a.ID, event.TodoChanged, event.TodoPayload{Items: items}))
	return err
}
