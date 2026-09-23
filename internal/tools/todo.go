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
	// items, as one change; an unknown id changes nothing. It returns the
	// list after the change.
	Edit(add []TodoAdd, updates []TodoUpdate) ([]event.TodoItem, error)
	List() []event.TodoItem
}

// TodoAdd is one new step. A step may start at any status, so planning
// work and starting its first step is one call rather than two.
type TodoAdd struct {
	Text   string `json:"text" desc:"The step, imperative and short (\"Run the tests\")" req:"true"`
	Status string `json:"status" desc:"The status to start it in: pending (the default), in_progress, done, cancelled"`
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
	return model.ToolDef{Name: toolname.Todo, Description: "Your todo list, the plan the human sees beside your chat. Use it for work with three or more steps: add the steps up front, short and imperative, then keep the list honest by id. Keep exactly one item in_progress while you work; mark an item done the moment it is finished and verified, never before; cancel steps you drop; add a new item for a blocker instead of marking the blocked step done. One call carries every change you have: post the plan and start its first step together, and finish one step and start the next in the same call, never two. Updates apply before the additions, and the whole list comes back with its ids. Skip the list for single-step or trivial requests.",
		Schema: schemaOf(todoInput{})}
}

type todoInput struct {
	Add    []TodoAdd    `json:"add" desc:"New steps to append, in order; each may start at a status, so a plan and its first in_progress step are one call"`
	Update []TodoUpdate `json:"update" desc:"Changes to existing items, by id: a status (pending | in_progress | done | cancelled), a new text, or both"`
}

func (todoTool) Subject(in json.RawMessage) policy.Subject {
	var a todoInput
	_ = decode(in, &a)
	values := make([]string, 0, len(a.Add)+len(a.Update))
	for _, it := range a.Add {
		values = append(values, it.Text)
	}
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
	add := make([]TodoAdd, 0, len(a.Add))
	for _, it := range a.Add {
		text := strings.TrimSpace(it.Text)
		switch {
		case text == "":
			return errf("add: a step needs text")
		case it.Status != "" && !validTodoStatus(it.Status):
			return errf("add %q: status must be one of %s", text, todoStatusNames())
		}
		add = append(add, TodoAdd{Text: text, Status: it.Status})
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
	// What changed, and the count: the whole list comes with every request
	// already, and echoing it here cost one channel 3.9 MB of context.
	var b strings.Builder
	done := 0
	for _, it := range items {
		if it.Status == event.TodoDone {
			done++
		}
	}
	for _, it := range items[max(0, len(items)-len(add)):] {
		fmt.Fprintf(&b, "added %s [%s] %s\n", it.ID, it.Status, it.Text)
	}
	for _, u := range a.Update {
		for _, it := range items {
			if it.ID == u.ID {
				fmt.Fprintf(&b, "%s → %s\n", it.ID, it.Status)
			}
		}
	}
	fmt.Fprintf(&b, "%d of %d done", done, len(items))
	return Result{Output: b.String()}
}
