package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"time"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
)

// operatorTool is deliberately outside the shell/MCP sandbox: it only sends
// a fixed request to the separate operator, which authenticates the kernel UID
// and a protected single-use grant. It offers no shell or credential access.
type operatorTool struct{}

type operatorInput struct {
	Grant      string `json:"grant" req:"true" desc:"32-character grant ID supplied by the protected operator"`
	Operation  string `json:"operation" req:"true" enum:"merge,delivery,host_report" desc:"Exactly the operation granted"`
	Repository string `json:"repository,omitempty" desc:"Exact owner/repository for merge or delivery"`
	Head       string `json:"head,omitempty" desc:"Full reviewed PR head for merge"`
	PR         int    `json:"pull_request,omitempty" desc:"Reviewed PR number for merge"`
	Revision   string `json:"revision,omitempty" desc:"Full merged revision for delivery status"`
	Host       string `json:"host,omitempty" desc:"Exact approved host for its sanitized read-only report"`
}

func (operatorTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.Operator, Description: "Use one finite grant supplied by the protected fleet operator. The separate operator verifies caller identity, exact request, expiry and single-use state, and authenticates CI before a merge. A failed or interrupted request consumes its grant; inspect its outcome before requesting a new one. This tool cannot issue grants, execute commands or read credentials.", Schema: schemaOf(operatorInput{})}
}

func (operatorTool) Subject(in json.RawMessage) policy.Subject {
	var input operatorInput
	_ = json.Unmarshal(in, &input)
	return policy.Text(input.Operation + " " + input.Repository + " " + input.Host + " " + input.Head + input.Revision)
}

func operatorMessage(in json.RawMessage) ([]byte, error) {
	if len(in) > 4096 {
		return nil, fmt.Errorf("operator input exceeds its bound")
	}
	var input operatorInput
	d := json.NewDecoder(bytes.NewReader(in))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(input.Grant) {
		return nil, fmt.Errorf("operator needs one exact finite grant")
	}
	request := map[string]any{"operation": input.Operation}
	switch input.Operation {
	case "merge":
		if input.PR <= 0 || input.Revision != "" || input.Host != "" {
			return nil, fmt.Errorf("merge scope differs")
		}
		request["repository"], request["head"], request["pull_request"] = input.Repository, input.Head, input.PR
	case "delivery":
		if input.PR != 0 || input.Head != "" || input.Host != "" {
			return nil, fmt.Errorf("delivery scope differs")
		}
		request["repository"], request["revision"] = input.Repository, input.Revision
	case "host_report":
		if input.PR != 0 || input.Head != "" || input.Revision != "" || input.Repository != "" || !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`).MatchString(input.Host) {
			return nil, fmt.Errorf("host report scope differs")
		}
		request["host"] = input.Host
	default:
		return nil, fmt.Errorf("unknown operator operation")
	}
	if input.Operation != "host_report" {
		revision := input.Head + input.Revision
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision) || !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(input.Repository) {
			return nil, fmt.Errorf("operator requires exact repository and revision")
		}
	}
	return json.Marshal(map[string]any{"grant": input.Grant, "request": request})
}

func operatorCall(ctx context.Context, socket string, body []byte, owner int) Result {
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0007 != 0 {
		return errf("protected fleet operator unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		return errf("protected fleet operator unavailable")
	}
	defer connection.Close()
	if err := operatorPeer(connection.(*net.UnixConn), owner); err != nil {
		return errf("protected fleet operator identity differs")
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(deadline)
	if _, err := connection.Write(body); err != nil {
		return errf("operator request interrupted; grant outcome requires inspection")
	}
	if err := connection.(*net.UnixConn).CloseWrite(); err != nil {
		return errf("operator request interrupted; grant outcome requires inspection")
	}
	response, err := io.ReadAll(io.LimitReader(connection, 65537))
	if err != nil || len(response) > 65536 {
		return errf("operator outcome unavailable; inspect its protected grant receipt before retrying")
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Reason string          `json:"reason"`
	}
	if json.Unmarshal(response, &envelope) != nil || envelope.OK && len(envelope.Result) == 0 {
		return errf("invalid operator outcome; inspect its protected grant receipt")
	}
	if !envelope.OK {
		return errf("operator refused: %s", envelope.Reason)
	}
	return Result{Output: string(envelope.Result)}
}

func (operatorTool) Run(ctx context.Context, in json.RawMessage, _ *Env) Result {
	body, err := operatorMessage(in)
	if err != nil {
		return errf("%v", err)
	}
	// A project cannot supply a socket, executable, policy or credential path.
	return operatorCall(ctx, "/run/fleet-operator/operator.sock", body, 0)
}

// RunOperator exposes the same bounded protocol to the local CLI for offline
// acceptance and owner operation, without starting an agent or provider call.
func RunOperator(ctx context.Context, input json.RawMessage) Result {
	return (operatorTool{}).Run(ctx, input, nil)
}
