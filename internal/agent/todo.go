package agent

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// The todo list is state in the log: every change is a todo.changed event
// carrying the whole list, so recovery replays the last one and every
// client renders the same list. Tool calls on one agent run one at a time,
// so a snapshot built under the lock is applied after it is logged.

func (a *Agent) todosAPI() tools.Todos { return todosAPI{a: a} }

// todoAPIIfEnabled is the list for the tool env, nil when the preset does
// not include "todo" (the tools then report themselves unavailable).
func (a *Agent) todoAPIIfEnabled() tools.Todos {
	if contains(a.preset.Tools, "todo") {
		return a.todosAPI()
	}
	return nil
}

type todosAPI struct{ a *Agent }

func (t todosAPI) Add(text string) (string, error) {
	a := t.a
	a.mu.Lock()
	a.todoSeq++
	id := "t" + strconv.Itoa(a.todoSeq)
	items := append(a.todosCopy(), event.TodoItem{ID: id, Text: text, Status: event.TodoPending})
	a.mu.Unlock()
	return id, a.setTodos(items)
}

func (t todosAPI) Update(id, status, text string) error {
	a := t.a
	a.mu.Lock()
	items := a.todosCopy()
	i := -1
	for k, it := range items {
		if it.ID == id {
			i = k
		}
	}
	if i < 0 {
		a.mu.Unlock()
		return fmt.Errorf("no todo item %q", id)
	}
	if status != "" {
		items[i].Status = event.TodoStatus(status)
	}
	if text != "" {
		items[i].Text = text
	}
	a.mu.Unlock()
	return a.setTodos(items)
}

func (t todosAPI) List() []event.TodoItem {
	t.a.mu.Lock()
	defer t.a.mu.Unlock()
	return t.a.todosCopy()
}

// todosCopy returns the list; the caller holds a.mu.
func (a *Agent) todosCopy() []event.TodoItem {
	return append([]event.TodoItem(nil), a.todos...)
}

// setTodos logs the new list and installs it once logged.
func (a *Agent) setTodos(items []event.TodoItem) error {
	if _, err := a.record(a.ctx, event.TodoChanged, event.TodoPayload{Items: items}); err != nil {
		return err
	}
	a.mu.Lock()
	a.todos = items
	a.mu.Unlock()
	return nil
}

// restoreTodos installs a replayed snapshot and moves the id counter past
// every id in it.
func (a *Agent) restoreTodos(items []event.TodoItem) {
	a.todos = items
	for _, it := range items {
		if n, err := strconv.Atoi(strings.TrimPrefix(it.ID, "t")); err == nil && n > a.todoSeq {
			a.todoSeq = n
		}
	}
}
