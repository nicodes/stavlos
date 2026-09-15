package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
)

// Todos is implemented by the agent runtime: the agent's own todo list,
// logged on every change and shown to the human beside the chat.
type Todos interface {
	// Edit applies updates to existing items, then appends add as new
	// pending items, as one change; an unknown id changes nothing. It
	// returns the list after the change.
	Edit(add []string, updates []TodoUpdate) ([]event.TodoItem, error)
	List() []event.TodoItem
}

// TodoUpdate changes one item: its status, its text, or both.
type TodoUpdate struct {
	ID     string `json:"id" desc:"The item's id (t1, t2, …)" req:"true"`
	Status string `json:"status" desc:"pending | in_progress | done | cancelled"`
	Text   string `json:"text" desc:"New text for the item"`
}

// TodoStatuses are the states a todo item moves through.
var TodoStatuses = []event.TodoStatus{event.TodoPending, event.TodoInProgress, event.TodoDone, event.TodoCancelled}

func todoStatusNames() string {
	names := make([]string, 0, len(TodoStatuses))
	for _, s := range TodoStatuses {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

func validTodoStatus(s string) bool {
	for _, v := range TodoStatuses {
		if string(v) == s {
			return true
		}
	}
	return false
}

type todoTool struct{}

func (todoTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.Todo, Description: "Your todo list, the plan the human sees beside your chat. Use it for work with three or more steps: add the steps up front, short and imperative, then keep the list honest with update, by id. Keep exactly one item in_progress while you work; mark an item done the moment it is finished and verified, never before; cancel steps you drop; add a new item for a blocker instead of marking the blocked step done. One call can update items and add new ones. Returns the whole list with ids. Skip the list for single-step or trivial requests.",
		Schema: schemaOf(todoInput{})}
}

type todoInput struct {
	Add    []string     `json:"add" desc:"New steps to append, in order, each imperative and short (\"Run the tests\")"`
	Update []TodoUpdate `json:"update" desc:"Changes to existing items, by id: a status (pending | in_progress | done | cancelled), a new text, or both"`
}

func (todoTool) Subject(in json.RawMessage) policy.Subject {
	var a todoInput
	_ = decode(in, &a)
	values := append([]string(nil), a.Add...)
	for _, u := range a.Update {
		values = append(values, u.ID)
	}
	return policy.Text(strings.Join(values, "\n"))
}

func (todoTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if env.Todo == nil {
		return errf("the todo list is not available to this agent")
	}
	var a todoInput
	if err := decode(in, &a); err != nil {
		return errf("%v", err)
	}
	if len(a.Add) == 0 && len(a.Update) == 0 {
		return errf("nothing to do: give add, update, or both")
	}
	add := make([]string, 0, len(a.Add))
	for _, text := range a.Add {
		if text = strings.TrimSpace(text); text == "" {
			return errf("add: a step needs text")
		}
		add = append(add, text)
	}
	for i, u := range a.Update {
		switch {
		case u.ID == "":
			return errf("update: every change needs the item's id")
		case u.Status != "" && !validTodoStatus(u.Status):
			return errf("update %s: status must be one of %s", u.ID, todoStatusNames())
		case u.Status == "" && strings.TrimSpace(u.Text) == "":
			return errf("update %s: nothing to change: give a status, a text, or both", u.ID)
		}
		a.Update[i].Text = strings.TrimSpace(u.Text)
	}
	items, err := env.Todo.Edit(add, a.Update)
	if err != nil {
		return errf("%v", err)
	}
	var b strings.Builder
	b.WriteString("todo list:")
	for _, it := range items {
		fmt.Fprintf(&b, "\n- %s [%s] %s", it.ID, it.Status, it.Text)
	}
	return Result{Output: b.String()}
}
