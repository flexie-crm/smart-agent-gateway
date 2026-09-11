// Package cmdpolicy decides what a command may do, wherever it runs.
//
// It was the SSH tool's, and it is not about SSH: the question "may this command
// run" is the same on a server somebody configured and on the person's own
// computer, and answering it twice would mean two parsers, two sets of rules an
// administrator has to learn, and two places for a way round them to appear.
package cmdpolicy

import (
	"fmt"
	"path"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/syntax"
)

// What may run on the server.
//
// The command is not matched as text. It is parsed the way a shell parses it, so
// what is checked is what would actually run: every command in a pipeline, in a
// subshell, behind a && , inside a substitution. Matching text instead is how
// "ls; rm -rf /" gets past a list that permits ls.
//
// The rule is the same one the query tool's access gate follows: allow only what
// is recognised. A command whose words cannot be read literally (they are built
// by a substitution, so what runs is decided on the server) is refused, because
// nothing here could honestly say what it would do.

// PolicyMode is how the list is read: an allowlist permits only what it names,
// a denylist permits everything except what it names.
type PolicyMode string

const (
	PolicyAllowlist PolicyMode = "allowlist"
	PolicyDenylist  PolicyMode = "denylist"
)

// Sudo settings. Running as another user is refused by default, and the setting
// applies to a denylist: an allowlist already names everything that may run.
const (
	SudoDeny  = "deny"
	SudoAllow = "allow"
)

// DefaultDenied is the denylist every tool that runs commands starts with: the
// programs whose damage cannot be undone, or cannot be reached afterwards.
//
// ONE list, because there is one question. The server tool had its own and the
// terminal was given a second, which is two lists that answer "what is too
// dangerous to run" and will disagree within a release: somebody hardens one
// after an incident and the other keeps the hole. A name that means nothing on
// a given machine costs nothing, so the union is cheaper than the split.
//
// It is a guard against a mistake, or against an agent doing what something it
// read told it to, and it is NOT a wall: anything that can write a script and
// run it goes around it. What actually bounds these tools is the account they
// run as, which on a server is the one it signs in as and on somebody's own
// computer is them.
//
// The principle for what belongs: irreversible, or it takes the machine away
// from the person. Common and recoverable does not belong however alarming it
// sounds, so chmod, chown, kill and mv are not here: a tool that cannot do
// ordinary work is a tool people switch off, and then nothing is denied at all.
//
// Grouped with blank lines and headed with #, both of which the lists skip, so
// an administrator reading the form can see why each one is there.
//
// It is a STARTING POINT and not a rule. Every line can be taken out by whoever
// owns the tool.
const DefaultDenied = `# Erasing things
rm
rmdir
shred
srm

# Writing over disks and filesystems
dd
wipefs
mkswap
mkfs
mkfs.ext2
mkfs.ext3
mkfs.ext4
mkfs.xfs
mkfs.btrfs
mkfs.vfat
mkfs.ntfs
newfs
fdisk
parted
diskutil
format
diskpart

# Taking the machine down
shutdown
reboot
halt
poweroff
init

# Locking you out of it
iptables
ip6tables
nft
ufw
firewall-cmd

# Accounts and who may do what
passwd
chpasswd
useradd
userdel
usermod
groupdel
dscl

# Windows, where the same damage has other names
del
erase
rd
vssadmin
bcdedit
takeown`

// privilegeWrappers run the rest of the command as somebody else. They are
// looked through, so a denied command cannot be smuggled behind one.
var privilegeWrappers = map[string]bool{"sudo": true, "doas": true, "su": true}

// Policy is what an administrator permits on a server. The lists are kept as the
// administrator typed them, one entry per line, so the edit form shows back
// exactly what was entered.
type Policy struct {
	Mode    PolicyMode `json:"mode"`
	Allowed string     `json:"allowed,omitempty"`
	Denied  string     `json:"denied,omitempty"`
	Sudo    string     `json:"sudo,omitempty"`
}

// shells are the programs whose input is commands. Starting one is the same
// grant as running commands, so what is typed into it is checked exactly as a
// command is; starting anything else means the input belongs to that program.
var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "ksh": true, "dash": true,
	"ash": true, "fish": true, "csh": true, "tcsh": true, "rbash": true,
}

// IsShell reports whether starting this command opens something that reads
// commands. It is decided from what is started, and recorded then, never
// guessed at afterwards from what somebody typed.
func IsShell(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		// A session with no command at all is a login shell.
		return true
	}
	return shells[path.Base(fields[0])]
}

// checkInput decides whether a line may be typed into a running session.
//
// Into a shell it is a command, and gets exactly the check a command gets: the
// parser, the whole AST, every program the line would run. Into anything else it
// is that program's own language, which nothing here can parse, so all that is
// left is to refuse a line that mentions a denied program anywhere. That is a
// conservative refusal rather than an analysis: it stops the obvious attempts to
// reach a denied program through a program's own escape, and it stops nothing
// that has been written to a file and run. See the guide.
// CheckInput decides whether a line may be typed into a running program.
func (p Policy) CheckInput(input string, shell bool) string {
	if shell {
		return p.Check(input)
	}
	for _, entry := range p.DeniedEntries() {
		if mentions(entry[0], input) {
			return fmt.Sprintf("this input mentions %q, which this tool refuses. Report that it is not permitted "+
				"rather than trying another way to reach it.", entry[0])
		}
	}
	return ""
}

// mentions reports whether a line names a program anywhere in it, whatever
// punctuation surrounds it, so an escape a program offers (!rm, :!rm, \! rm)
// does not get a denied program past the check simply by not being shell syntax.
func mentions(program, line string) bool {
	for _, word := range strings.FieldsFunc(line, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' && r != '/'
	}) {
		if word == program || path.Base(word) == program {
			return true
		}
	}
	return false
}

// AllowedEntries and DeniedEntries are the lists as written, one command per
// line broken into words, for anything that has to describe the policy rather
// than enforce it.
func (p Policy) AllowedEntries() [][]string { return entries(p.Allowed) }
func (p Policy) DeniedEntries() [][]string  { return entries(p.Denied) }

// entries splits a list into one command per line, each broken into words. A
// blank line is skipped so an administrator can space the list out, and so is a
// line starting with #, so they can say what a group of them is for.
//
// The comment is not decoration. These lists are read by people who did not
// write them and are shown to the model in the tool's guide, and a list of
// thirty program names with no headings is a list nobody edits with confidence.
// Without this the heading itself became an entry: harmless to match, since no
// program is called #, and wrong everywhere it is displayed.
func entries(list string) [][]string {
	var out [][]string
	for _, line := range strings.Split(list, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			out = append(out, fields)
		}
	}
	return out
}

// withDefaults settles the policy an administrator left blank on the safer
// reading: only what is named may run, and nothing runs as another user.
// WithDefaults fills in what an administrator left blank.
func (p Policy) WithDefaults() Policy {
	if p.Mode == "" {
		p.Mode = PolicyAllowlist
	}
	if p.Sudo == "" {
		p.Sudo = SudoDeny
	}
	return p
}

// Validate refuses a policy that cannot be enforced as written.
func (p Policy) Validate() error {
	switch p.Mode {
	case PolicyAllowlist:
		if len(p.AllowedEntries()) == 0 {
			return fmt.Errorf("list the commands this tool may run, one per line")
		}
	case PolicyDenylist:
	default:
		return fmt.Errorf("the command policy must be allowlist or denylist")
	}
	switch p.Sudo {
	case SudoDeny, SudoAllow:
	default:
		return fmt.Errorf("sudo must be deny or allow")
	}
	return nil
}

// check reads the command and decides whether it may run. It returns an empty
// string when it may, and otherwise the reason, written for the assistant to
// report rather than work around.
// Check decides whether a command may run: the empty string when it may, and
// the reason to tell the assistant when it may not.
func (p Policy) Check(command string) string {
	if strings.TrimSpace(command) == "" {
		return "no command was given"
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return "the command could not be read as a command, so it was not run"
	}

	switch p.Mode {
	case PolicyAllowlist:
		return p.checkAllowed(file)
	case PolicyDenylist:
		return p.checkDenied(file)
	default:
		return "this tool has no command policy, so nothing was run"
	}
}

// checkAllowed permits one plain command, named in the list. Anything with more
// moving parts (a pipeline, a redirection, a second statement, a substitution) is
// refused: the list names commands, and a construction is not one of them.
func (p Policy) checkAllowed(file *syntax.File) string {
	if len(file.Stmts) != 1 {
		return "run one command per call; this tool does not accept several commands joined together"
	}
	stmt := file.Stmts[0]
	if stmt.Background || stmt.Negated || len(stmt.Redirs) > 0 {
		return "this tool runs a plain command: no redirection, no backgrounding, no negation"
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok {
		return "this tool runs a plain command, not a pipeline, a loop, or a subshell"
	}
	if len(call.Assigns) > 0 {
		return "this tool runs a plain command, without variables set in front of it"
	}

	// Every command this line would run, which is the call itself plus anything
	// inside a substitution: `ls $(rm -rf /)` runs two things, and permitting the
	// first says nothing about the second.
	for _, run := range everyCommand(stmt.Cmd) {
		if reason := p.allows(run); reason != "" {
			return reason
		}
	}
	return ""
}

// allows checks one command's words against the allowlist.
func (p Policy) allows(words []string) string {
	if len(words) == 0 {
		return "no command was given"
	}
	// The NAME has to be known, because an allowlist is a list of names and a
	// name the shell decides at run time cannot be looked up in it.
	if words[0] == Unknown {
		return "this command's name is put together on the server, so what would run cannot be checked; write it out in full"
	}
	// And for anything that runs what it is handed, its arguments are a command
	// too, so they have to be known as well.
	if runsWhatItIsGiven[path.Base(words[0])] {
		for _, word := range words[1:] {
			if word == Unknown {
				return fmt.Sprintf("%s runs what it is given, so its arguments are put together on the "+
					"server and cannot be checked; write them out in full", words[0])
			}
		}
	}
	allowed := p.AllowedEntries()
	// An allowlist with nothing on it permits nothing, and saying "X is not one
	// of the commands this tool permits" about a list that names none reads as a
	// rule somebody wrote about X. Said here because this is what knows what a
	// mode means; the tools that ask are not supposed to know there are modes.
	if len(allowed) == 0 {
		return "this tool is set to permit only the commands on its list, and its list is empty, " +
			"so it runs nothing. Report that rather than trying another command."
	}
	for _, entry := range allowed {
		if matchesAllowed(entry, words) {
			return ""
		}
	}
	return fmt.Sprintf("%q is not one of the commands this tool permits. The permitted ones are fixed by its "+
		"configuration, so rewording will not help; report that it is not permitted.", strings.Join(words, " "))
}

// checkDenied permits anything except what is named, and looks at every command
// the line would run, not just the first.
func (p Policy) checkDenied(file *syntax.File) string {
	denied := p.DeniedEntries()
	reason := ""

	syntax.Walk(file, func(node syntax.Node) bool {
		if reason != "" {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		words := readWords(call.Args)
		if len(words) == 0 {
			return true
		}
		// The NAME, because a denylist is a list of names: one the shell decides
		// at run time could be any of them, including the ones named here.
		if words[0] == Unknown {
			reason = "this command's name is put together on the server, so what would run cannot be checked; write it out in full"
			return false
		}
		// An argument is data and cannot run, UNLESS the command runs what it is
		// handed, which is the whole of how a denied command gets in sideways:
		// `xargs $CMD` and `sh -c "$CMD"` say nothing about what they will run.
		if runsWhatItIsGiven[path.Base(words[0])] {
			for _, word := range words[1:] {
				if word == Unknown {
					reason = fmt.Sprintf("%s runs what it is given, so its arguments are put together on "+
						"the server and cannot be checked; write them out in full", words[0])
					return false
				}
			}
		}
		// Running as somebody else is its own decision, whatever the command.
		if privilegeWrappers[path.Base(words[0])] && p.Sudo != SudoAllow {
			reason = fmt.Sprintf("running commands as another user (%s) is not permitted by this tool", words[0])
			return false
		}
		for _, entry := range denied {
			if matchesDenied(entry, words) {
				reason = fmt.Sprintf("this command runs %q, which this tool refuses. Report that it is not permitted "+
					"rather than trying another way to run it.", strings.Join(entry, " "))
				return false
			}
		}
		return true
	})
	return reason
}

// everyCommand reads every command a node would run: the call itself, and any
// command inside a substitution in its words.
//
// A substitution is why an argument can be relaxed at all. `$(rm -rf /)` is not
// a value the shell computes, it is a command that runs and whose output becomes
// the value, so the safe thing is not to refuse the word but to check what is
// inside it exactly as if it had been written on its own line. Nested ones come
// out too, because `$(echo $(rm -rf /))` is two.
func everyCommand(node syntax.Node) [][]string {
	var out [][]string
	syntax.Walk(node, func(n syntax.Node) bool {
		if call, ok := n.(*syntax.CallExpr); ok {
			out = append(out, readWords(call.Args))
		}
		return true
	})
	return out
}

// matchesAllowed reports whether an allowlist entry describes this command. It
// matches from the first word and compares words exactly: an entry naming a
// program permits it called by that name, and an entry naming a path permits
// that path. Matching a bare name against a path (or the other way round) would
// let a program of the same name somewhere else stand in for the permitted one.
func matchesAllowed(entry, words []string) bool {
	if len(entry) == 0 || len(entry) > len(words) {
		return false
	}
	for i := range entry {
		if entry[i] != words[i] {
			return false
		}
	}
	return true
}

// matchesDenied reports whether a denylist entry appears anywhere in this
// command, by the name given or by any path ending in it.
//
// It matches loosely on purpose, the opposite way round from the allowlist: a
// denied command is reached as an argument at least as often as at the front
// ("xargs rm -rf", "sudo -u deploy rm", "find . -exec rm {} ;"), and a denylist
// that only looked at the first word would miss every one of them. The cost is
// that a denied word is refused even where it is only being talked about, which
// is the right way for this one to be wrong.
func matchesDenied(entry, words []string) bool {
	if len(entry) == 0 {
		return false
	}
	for start := 0; start+len(entry) <= len(words); start++ {
		if !sameProgram(entry[0], words[start]) {
			continue
		}
		matched := true
		for i := 1; i < len(entry); i++ {
			if entry[i] != words[start+i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// sameProgram reports whether a word names the program a denylist entry names,
// as itself or as the last part of a path, so naming rm also refuses /bin/rm.
func sameProgram(entry, word string) bool {
	return entry == word || entry == path.Base(word)
}

// literalWords reads a command's words when every one of them is written out.
// It reports false as soon as one is not: a word built from a variable or from
// another command's output is decided on the server, and a check that guessed at
// it would be a check in name only.
// Unknown stands for a word whose value the shell decides: a variable, a glob,
// a piece of arithmetic. It is deliberately something no command and no entry
// can ever be, so it matches nothing rather than matching loosely.
const Unknown = "\x00unknown"

// literalWords reads a command's words, and is the strict form: every word must
// be known. Used where the command NAME is being read.
func literalWords(args []*syntax.Word) ([]string, bool) {
	words := make([]string, 0, len(args))
	for _, arg := range args {
		word, ok := literalWord(arg)
		if !ok {
			return nil, false
		}
		words = append(words, word)
	}
	return words, true
}

// readWords is the same, except that a word the shell will decide is reported as
// Unknown rather than refusing the whole command.
//
// The strict form refuses `ls *.log` and `grep "$pattern" file`, which are the
// ordinary shape of using a shell, and it refused them for a reason that only
// applies to the FIRST word: we cannot check a command whose name we do not
// know. An argument is not a command. `*.log` becomes filenames, `$pattern`
// becomes text, `$((1+2))` becomes a number, and none of them can run anything.
//
// A command substitution is the exception and it is handled elsewhere rather
// than here: `$(rm -rf /)` really does run something, and what it runs is a
// command of its own that the walker visits and checks against this same policy
// (see everyCommand). So it is Unknown as a VALUE, which is true, and not a
// hole, because the command inside it is checked in its own right.
func readWords(args []*syntax.Word) []string {
	words := make([]string, 0, len(args))
	for _, arg := range args {
		if word, ok := literalWord(arg); ok {
			words = append(words, word)
			continue
		}
		words = append(words, Unknown)
	}
	return words
}

// runsWhatItIsGiven names the commands that take another command as an argument
// and run it. For those, an argument is not data and the reasoning above does
// not hold, so their arguments must be known.
//
// This is the one place the relaxation would otherwise open a hole: `xargs $CMD`
// or `sh -c "$CMD"` turns a variable into something that runs, and no amount of
// reading the words of THIS command would see it.
var runsWhatItIsGiven = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"env": true, "xargs": true, "nohup": true, "timeout": true, "watch": true,
	"nice": true, "ionice": true, "setsid": true, "stdbuf": true,
	"find": true, "eval": true, "exec": true, "command": true,
	"sudo": true, "doas": true, "su": true,
}

func literalWord(word *syntax.Word) (string, bool) {
	var out strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			out.WriteString(p.Value)
		case *syntax.SglQuoted:
			out.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				out.WriteString(lit.Value)
			}
		default:
			// A parameter, a substitution, an arithmetic expression, a glob: what
			// this becomes is the server's decision, not ours.
			return "", false
		}
	}
	return out.String(), true
}
