package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

// Todos is implemented by the agent runtime: the agent's own todo list,
// logged on every change and shown to the human beside the chat.
type Todos interface {
	Add(text string) (string, error)
	Update(id, status, text string) error
	List() []event.TodoItem
}

// TodoStatuses are the states a todo item moves through.
var TodoStatuses = []string{"pending", "in_progress", "done", "cancelled"}

func validTodoStatus(s string) bool {
	for _, v := range TodoStatuses {
		if v == s {
			return true
		}
	}
	return false
}

type todoAddTool struct{}

func (todoAddTool) Def() model.ToolDef {
	return model.ToolDef{Name: "todo_add", Description: "Add one step to your todo list, the plan the human sees beside your chat. Use it for work with three or more steps: add the steps up front, short and imperative, then keep exactly one in_progress with todo_update as you go. Returns the item's id. Skip the list for single-step or trivial requests.",
		Schema: schema(map[string]any{"text": prop("string", "The step, imperative and short (\"Run the tests\")")}, "text")}
}

func (todoAddTool) PolicyArg(in json.RawMessage) string {
	var a struct{ Text string }
	_ = decode(in, &a)
	return a.Text
}

func (todoAddTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if env.Todo == nil {
		return errf("the todo list is not available to this agent")
	}
	var a struct{ Text string }
	if err := decode(in, &a); err != nil {
		return errf("%v", err)
	}
	text := strings.TrimSpace(a.Text)
	if text == "" {
		return errf("text is required")
	}
	id, err := env.Todo.Add(text)
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: fmt.Sprintf("added %s: %s", id, text)}
}

type todoUpdateTool struct{}

func (todoUpdateTool) Def() model.ToolDef {
	return model.ToolDef{Name: "todo_update", Description: "Update one item on your todo list: set its status (pending, in_progress, done, cancelled) and/or rewrite its text. Mark an item in_progress when you start it and done the moment it is finished and verified, never before; cancel steps you drop. Add a new item for a blocker instead of marking the blocked step done.",
		Schema: schema(map[string]any{
			"id":     prop("string", "The item id returned by todo_add"),
			"status": prop("string", "pending | in_progress | done | cancelled"),
			"text":   prop("string", "New text for the item (optional)"),
		}, "id")}
}

func (todoUpdateTool) PolicyArg(in json.RawMessage) string { return idArg(in) }

func (todoUpdateTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if env.Todo == nil {
		return errf("the todo list is not available to this agent")
	}
	var a struct{ ID, Status, Text string }
	if err := decode(in, &a); err != nil {
		return errf("%v", err)
	}
	if a.Status != "" && !validTodoStatus(a.Status) {
		return errf("status must be one of %s", strings.Join(TodoStatuses, ", "))
	}
	if a.Status == "" && strings.TrimSpace(a.Text) == "" {
		return errf("nothing to change: give a status, a text, or both")
	}
	if err := env.Todo.Update(a.ID, a.Status, strings.TrimSpace(a.Text)); err != nil {
		return errf("%v", err)
	}
	out := "updated " + a.ID
	if a.Status != "" {
		out += " → " + a.Status
	}
	return Result{Output: out}
}
