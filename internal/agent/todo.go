package agent

import (
	"context"
	"fmt"
	"strconv"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/toolname"
	"github.com/nicodes/stavlos/internal/tools"
)

// The todo list is state in the log: every change is a todo.changed event
// carrying the whole list, applied like any other, so recovery restores it
// and every client renders the same list.

// todoAPIFor is the list for a tool env, nil when the role does not
// include "todo" (the tools then say they are unavailable).
func (a *Agent) todoAPIFor(rv roleView) tools.Todos {
	if contains(rv.preset.Tools, toolname.GroupTodo) {
		return todosAPI{a: a}
	}
	return nil
}

type todosAPI struct{ a *Agent }

func (t todosAPI) Add(text string) (string, error) {
	var id string
	err := t.change(func(st *agentState, items []event.TodoItem) ([]event.TodoItem, error) {
		id = "t" + strconv.Itoa(st.todoSeq+1)
		return append(items, event.TodoItem{ID: id, Text: text, Status: event.TodoPending}), nil
	})
	return id, err
}

func (t todosAPI) Update(id, status, text string) error {
	return t.change(func(_ *agentState, items []event.TodoItem) ([]event.TodoItem, error) {
		for i := range items {
			if items[i].ID != id {
				continue
			}
			if status != "" {
				items[i].Status = event.TodoStatus(status)
			}
			if text != "" {
				items[i].Text = text
			}
			return items, nil
		}
		return nil, fmt.Errorf("no todo item %q", id)
	})
}

func (t todosAPI) List() []event.TodoItem {
	t.a.s.mu.Lock()
	defer t.a.s.mu.Unlock()
	return append([]event.TodoItem(nil), t.a.state().todos...)
}

// change logs the list edit makes of a copy of the current one.
func (t todosAPI) change(edit func(*agentState, []event.TodoItem) ([]event.TodoItem, error)) error {
	s := t.a.s
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
