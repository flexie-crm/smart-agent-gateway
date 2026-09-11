package api

import (
	"encoding/json"
	"testing"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/tool"
)

// What a person sees when they open a call is the TOOL's account of itself.
//
// The chat used to decide by field name, which meant the chat knew that a
// terminal answers with `exit_code` and a server tool with `running`. It does
// not any more: a tool declares what is worth reading (tool.Display), the
// server applies it, and adding a tool that answers with six fields nobody
// reads is a change in that tool.

// terminalLike is the declaration the terminal actually carries.
var terminalLike = tool.Display{
	Where:    []string{"directory"},
	Sent:     []tool.Shown{tool.Command("command")},
	Answered: []tool.Shown{tool.Text("stdout"), tool.Text("stderr")},
}

func names(fields []shownField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Name)
	}
	return out
}

func equal(t *testing.T, got, want []string, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
}

// present calls the server's own rule, so this cannot pass while the endpoint
// does something else. What the lookup decides (which declaration) is the
// endpoint test's business; this is what is done with one.
func present(display tool.Display, status string, args, result string) (where, sent, answered []shownField) {
	return presentUnder(display, &model.ToolCall{
		Status: status,
		Args:   json.RawMessage(args),
		Result: json.RawMessage(result),
	})
}

// presentFailing is the same, for a call that went wrong: the error text is
// what the panel shows in its own section, so nothing else may repeat it.
func presentFailing(display tool.Display, errText, args, result string) (where, sent, answered []shownField) {
	return presentUnder(display, &model.ToolCall{
		Status:    model.ToolCallFailed,
		ErrorText: errText,
		Args:      json.RawMessage(args),
		Result:    json.RawMessage(result),
	})
}

// The reported case: a command, where it ran, and what it printed. The exit
// code and the empty stderr are for the model.
func TestASuccessfulCallShowsWhatTheToolNamed(t *testing.T) {
	where, sent, answered := present(terminalLike, model.ToolCallCompleted,
		`{"command":"date -u"}`,
		`{"directory":"/w","exit_code":0,"stdout":"2026-09-03","stderr":""}`)

	equal(t, names(where), []string{"directory"}, "where it ran")
	equal(t, names(sent), []string{"command"}, "what was sent")
	equal(t, names(answered), []string{"stdout"}, "what came back")
	if sent[0].As != string(tool.ShownCommand) {
		t.Fatalf("the command is not marked as one: %+v", sent[0])
	}
	if answered[0].As != string(tool.ShownText) {
		t.Fatalf("the output is not marked as one: %+v", answered[0])
	}
}

// A failure is read by a person who wants to know what went wrong: the
// command, one error line (its own section), and what it printed. The whole
// payload instead was this panel being careful, and being careful produced the
// error twice with a success flag and an exit code around it.
func TestAFailedCallReadsAsWhatWentWrong(t *testing.T) {
	where, sent, answered := presentFailing(terminalLike, "no such option",
		`{"command":"date -u","directory":"/w"}`,
		`{"directory":"/w","exit_code":2,"success":false,"stdout":"","stderr":"date: no such option"}`)

	equal(t, names(where), []string{"directory"}, "where it ran")
	equal(t, names(sent), []string{"command"}, "what was sent")
	equal(t, names(answered), []string{"stderr"}, "what came back")
}

// But nobody is handed a blank. When the tool's own account leaves nothing to
// show on a failure, the rest is shown rather than an empty panel.
func TestAFailureWithNothingDeclaredToShowFallsBack(t *testing.T) {
	_, _, answered := presentFailing(terminalLike, "the machine is not connected",
		`{"command":"ls"}`, `{"reason":"no link to that computer"}`)

	equal(t, names(answered), []string{"reason"}, "what came back")
}

// A tool that has declared nothing is shown whole, because a tool nobody has
// thought about has not been thought about, and hiding a field somebody needed
// is the worse way to be wrong.
func TestAToolThatSaysNothingIsShownWhole(t *testing.T) {
	_, sent, answered := present(tool.Display{}, model.ToolCallCompleted,
		`{"url":"https://example.com","method":"GET"}`,
		`{"status":200,"body":"ok"}`)

	equal(t, names(sent), []string{"method", "url"}, "what was sent")
	equal(t, names(answered), []string{"body", "status"}, "what came back")
}

// What a call was about is shown at the top, from whichever side carries it.
//
// The reported failure: a read of a file showed an offset and a limit and never
// said WHICH FILE. The model had asked with an absolute path and the tool
// answered with the short one, so the two disagreed; the rule then kept
// neither, and a field that does not lift is not in the sent or answered lists
// either, so it fell through both.
//
// The ANSWER wins when both carry it. They usually differ only in spelling,
// and where it actually happened is the truthful half of that.
func TestWhatACallWasAboutIsAlwaysShown(t *testing.T) {
	reading := tool.Display{
		Where:    []string{"path"},
		Sent:     []tool.Shown{tool.Value("offset"), tool.Value("limit")},
		Answered: []tool.Shown{tool.Text("content")},
	}
	where, sent, answered := present(reading, model.ToolCallCompleted,
		`{"path":"/Users/me/p/app/service.py","offset":1,"limit":360}`,
		`{"path":"app/service.py","content":"import os"}`)

	equal(t, names(where), []string{"path"}, "what it was about")
	if string(where[0].Value) != `"app/service.py"` {
		t.Fatalf("it should say where it actually read: %s", where[0].Value)
	}
	equal(t, names(sent), []string{"offset", "limit"}, "what was sent")
	equal(t, names(answered), []string{"content"}, "what came back")
}

// And when only one side carries it, that is the one shown.
func TestWhereItRanComesFromEitherSide(t *testing.T) {
	// Asked for, and the tool does not repeat it.
	where, _, _ := present(terminalLike, model.ToolCallCompleted,
		`{"command":"ls","directory":"/asked"}`, `{"stdout":"a.txt"}`)
	equal(t, names(where), []string{"directory"}, "where it ran")
	if string(where[0].Value) != `"/asked"` {
		t.Fatalf("the only value there was is not the one shown: %s", where[0].Value)
	}

	// Not asked for, and the tool reports where it went.
	where, _, _ = present(terminalLike, model.ToolCallCompleted,
		`{"command":"ls"}`, `{"directory":"/actually","stdout":"a.txt"}`)
	equal(t, names(where), []string{"directory"}, "where it ran")
	if string(where[0].Value) != `"/actually"` {
		t.Fatalf("it should say where it went: %s", where[0].Value)
	}
}

// Nothing empty is shown: a label with a blank beside it is a question about
// the panel rather than about the call.
func TestAnEmptyFieldIsNotARow(t *testing.T) {
	_, _, answered := present(terminalLike, model.ToolCallCompleted,
		`{"command":"true"}`, `{"stdout":"","stderr":null,"directory":"/w"}`)
	if len(answered) != 0 {
		t.Fatalf("an empty answer was drawn as rows: %v", names(answered))
	}
}

// The error has a section of its own, so the result does not say it again.
//
// A failed call is shown whole, which is right, and the whole of it repeated
// the same sentence twice with a success flag that only said what the red row
// already said. That is three ways of reading one fact.
func TestAnErrorIsNotSaidTwice(t *testing.T) {
	_, _, answered := presentFailing(tool.Display{}, "no such file or directory",
		`{"command":"cat nope"}`,
		`{"success":false,"error":"no such file or directory","exit_code":1,"stderr":"cat: nope"}`)

	// A tool that declared nothing is shown whole, and "whole" still does not
	// mean the error twice with the row's own status beside it.
	equal(t, names(answered), []string{"exit_code", "stderr"}, "what came back")
}

// But a field that says MORE than the error line stays: on a call that went
// wrong the extra sentence is often the useful one.
func TestAnErrorThatElaboratesIsKept(t *testing.T) {
	_, _, answered := presentFailing(tool.Display{}, "the tool failed",
		`{}`, `{"error":"the tool failed","error_detail":"the server closed the connection after 30s"}`)

	equal(t, names(answered), []string{"error_detail"}, "what came back")
}

// And on a call that WORKED, a success flag agreeing with the row is the row
// again.
func TestASuccessFlagIsNotTheRowAgain(t *testing.T) {
	_, _, answered := present(tool.Display{}, model.ToolCallCompleted,
		`{}`, `{"success":true,"rows":3}`)

	equal(t, names(answered), []string{"rows"}, "what came back")
}

// A result set is one thing, not two fields.
//
// A database answers with the column names and the rows separately, because
// that is the compact way to carry it. Drawn as two fields it is a list of
// names above a list of lists, which is the shape of the data and not the
// shape of an answer, so the two are put together here and reach the page as
// a table.
func TestAResultSetArrivesAsATable(t *testing.T) {
	rows := tool.Display{
		Sent:     []tool.Shown{tool.Text("sql")},
		Answered: []tool.Shown{tool.Table("rows", "columns"), tool.Value("row_count")},
	}
	_, sent, answered := present(rows, model.ToolCallCompleted,
		`{"sql":"select id, name from people"}`,
		`{"columns":["id","name"],"rows":[[1,"Ada"],[2,"Grace"]],"row_count":2}`)

	equal(t, names(sent), []string{"sql"}, "what was sent")
	equal(t, names(answered), []string{"rows", "row_count"}, "what came back")
	if answered[0].As != string(tool.ShownTable) {
		t.Fatalf("the rows are not marked as a table: %+v", answered[0])
	}
	var table struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	if err := json.Unmarshal(answered[0].Value, &table); err != nil {
		t.Fatalf("the table is not readable: %v", err)
	}
	if len(table.Columns) != 2 || len(table.Rows) != 2 {
		t.Fatalf("the columns and the rows did not arrive together: %+v", table)
	}
	if table.Columns[1] != "name" || table.Rows[1][1] != "Grace" {
		t.Fatalf("the table lost its contents: %+v", table)
	}
}

// A value with nothing in it TO READ is not a row.
//
// A pager quit with `q` answers with a screen's worth of spaces. It is not the
// empty string, so it passed as a value, and the panel drew ANSWERED over
// something invisible: a heading with a void under it reads as a broken panel
// rather than as a command that printed nothing.
func TestWhitespaceIsNothingToRead(t *testing.T) {
	_, sent, answered := present(
		tool.Display{
			Sent:     []tool.Shown{tool.Value("input")},
			Answered: []tool.Shown{tool.Text("output")},
		},
		model.ToolCallCompleted,
		`{"input":"q"}`,
		`{"success":true,"output":"                              ","exit_code":0}`)

	equal(t, names(sent), []string{"input"}, "what was sent")
	if len(answered) != 0 {
		t.Fatalf("a screenful of spaces was drawn as an answer: %v", names(answered))
	}
}

// A tool projected from an MCP server is shown exactly as it answered.
//
// Its results are a third party's, in whatever shape that party chose, and
// nobody here can honestly say which of their fields matters. So there is no
// declaration, and no declaration means the whole answer: what a person reads
// is what the service sent, in the order it is in.
func TestAProjectedToolIsShownAsItAnswered(t *testing.T) {
	_, sent, answered := present(tool.Display{}, model.ToolCallCompleted,
		`{"entity":"contact","id":41}`,
		`{"contact":{"name":"Ada"},"matched":1,"provider_note":"cached"}`)

	equal(t, names(sent), []string{"entity", "id"}, "what was sent")
	equal(t, names(answered), []string{"contact", "matched", "provider_note"}, "what came back")
}
