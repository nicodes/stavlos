package policy

// Kind says what a tool call's policy subject is. Rules match the subject
// as text; the harness uses the kind to know what else the text means — a
// command that may be compound, a path that may leave the working set, a
// URL with a host to remember — without naming the tool.
type Kind int

const (
	KindText    Kind = iota // free text: a query, a question, a skill name, compact JSON
	KindCommand             // a shell command line
	KindPath                // a filesystem path (one or several)
	KindURL                 // a fetch target, in the form it will be fetched
	KindID                  // an agent, job or item id
)

// Subject is what policy judges for one tool call: every string a rule may
// match (the first is the primary one, shown in prompts), and its kind.
type Subject struct {
	Kind   Kind
	Values []string
}

// Primary is the subject's first value ("" when it has none).
func (s Subject) Primary() string {
	if len(s.Values) == 0 {
		return ""
	}
	return s.Values[0]
}

// Text, Command, Path, URL and ID build subjects of each kind.
func Text(v string) Subject        { return Subject{Kind: KindText, Values: []string{v}} }
func Command(v string) Subject     { return Subject{Kind: KindCommand, Values: []string{v}} }
func Path(paths ...string) Subject { return Subject{Kind: KindPath, Values: paths} }
func URL(v string) Subject         { return Subject{Kind: KindURL, Values: []string{v}} }
func ID(v string) Subject          { return Subject{Kind: KindID, Values: []string{v}} }
