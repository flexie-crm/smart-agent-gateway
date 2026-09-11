package sshtool

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"flexie.io/sag/internal/tool"
)

// Servers that ask for a verification code.
//
// Some servers want a second factor at sign-in: a code from an authenticator, a
// text message, an email. The server asks the question, not us, so there is
// nothing here that knows about any of those methods. What is here is the relay:
// the server's own question is carried to the person, and their answer is handed
// straight back to the sign-in waiting on it.
//
// The sign-in cannot be held on the call that started it, because a person needs
// a moment and a tool call must not sit there. So the sign-in moves to its own
// goroutine and the call returns saying a code is needed. The next call carries
// the code, gives it to the waiting sign-in, and then does the work it came to
// do. Which means: the code arrives as a tool argument, and is written straight
// into the connection. It is never stored, and nothing here logs it.

// defaultCodeWindow is how long a sign-in waits for a code. It is short on
// purpose: a server gives an unfinished sign-in a couple of minutes at the
// outside, and a code is usually only valid for about that long anyway. When it
// runs out the sign-in is abandoned and the next call starts a fresh one. The
// pool carries the value so a test can shorten it; it is never an administrator's
// to set.
const defaultCodeWindow = 60 * time.Second

var (
	// errNoPendingSignIn is a code arriving with nothing waiting for it: the
	// window ran out, or it was already used.
	errNoPendingSignIn = errors.New("no sign-in is waiting for a code")
	// errNotYourSignIn is a code offered for a question somebody else was asked.
	// The sign-in belongs to the agent it was raised by, so an answer from
	// another one cannot complete it.
	errNotYourSignIn = errors.New("this sign-in was not started here")
	// errSigningIn is another agent already signing in to this server. The call
	// waits rather than asking a second person for a second code.
	errSigningIn = errors.New("this server is being signed in to")
	// errCodeWindowClosed is nobody answering in time.
	errCodeWindowClosed = errors.New("the code was not given in time")
	// errVerificationNeeded reports that a server asked for a code, for the
	// administrator's Test, which has no person to ask.
	errVerificationNeeded = errors.New("the server asks for a verification code at sign-in")
)

// needsCode is the sign-in stopping to ask. It travels out through the pool to
// the handler, which turns it into the answer the assistant reads.
type needsCode struct {
	ID          string
	Prompt      string
	Instruction string
	Expires     time.Time
}

func (n *needsCode) Error() string { return "the server is waiting for a verification code" }

// Question is what to put to the person: the server's own wording, or a plain
// stand-in for a server that asked without saying anything.
func (n *needsCode) Question() string {
	if n.Prompt != "" {
		return n.Prompt
	}
	return "Verification code:"
}

// signIn is one attempt at connecting to a server, and the question it stopped
// on if it stopped on one. Everything that wants the connection waits on the
// same attempt, so a server is signed in to once however many calls arrive.
type signIn struct {
	done  chan struct{}
	asked chan struct{}
	once  sync.Once

	mu          sync.Mutex
	client      *ssh.Client
	err         error
	id          string
	prompt      string
	instruction string
	expires     time.Time
	delivered   bool
	code        chan string
	// owner is the agent the question was raised by. Only it can answer: the
	// pairing of this tool and that agent is the whole identity, which is why
	// nothing about it is handed to the model to carry around.
	owner tool.Owner
}

func newSignIn() *signIn {
	return &signIn{
		done:  make(chan struct{}),
		asked: make(chan struct{}),
		code:  make(chan string, 1),
	}
}

// challenge answers the server's sign-in questions: the password from the
// configuration, anything further from a person.
//
// Which is which is not read out of the wording, which the protocol does not
// label and which differs by server and by language. It follows from whether the
// server took the password directly. A server that accepts passwords has already
// had ours by the time it asks anything else, so what it is asking for now is a
// second factor and only a person has it. A server that does not accept them
// directly never asked for it, so the first thing it wants IS the password, and
// the tool answers that itself: a login credential is the tool's to hold, never
// something to put to a person or through the assistant.
func (s *signIn) challenge(window time.Duration, owner tool.Owner) questionHandler {
	return func(cfg Config, offered *passwordOffered) ssh.KeyboardInteractiveChallenge {
		return func(_, instruction string, questions []string, _ []bool) ([]string, error) {
			if len(questions) == 0 {
				// The server was telling us something, not asking.
				return nil, nil
			}

			answers := make([]string, len(questions))
			first := 0
			if cfg.answersFirstQuestion(offered) {
				answers[0] = cfg.Auth.Password
				first = 1
			}

			switch remaining := len(questions) - first; remaining {
			case 0:
				return answers, nil
			case 1:
				answer, err := s.askPerson(instruction, questions[first], window, owner)
				if err != nil {
					return nil, err
				}
				answers[first] = answer
				return answers, nil
			default:
				// Several things at once, beyond the password, need answering
				// together, and a person can only be put one question at a time
				// here. Rare, and better refused plainly than answered with a guess.
				return nil, fmt.Errorf("this server asks for several things at once when signing in, which this tool cannot answer")
			}
		}
	}
}

// askPerson raises the server's question and waits for the answer.
func (s *signIn) askPerson(instruction, prompt string, window time.Duration, owner tool.Owner) (string, error) {
	id, err := newCodeID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.id = id
	s.owner = owner
	s.prompt = strings.TrimSpace(prompt)
	s.instruction = strings.TrimSpace(instruction)
	s.expires = time.Now().Add(window)
	// A server that asks again (a code that was not accepted) is a new question,
	// so anything left from the last one is cleared.
	s.delivered = false
	select {
	case <-s.code:
	default:
	}
	s.mu.Unlock()

	s.once.Do(func() { close(s.asked) })

	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case code := <-s.code:
		return code, nil
	case <-timer.C:
		return "", errCodeWindowClosed
	}
}

// deliver hands a code to the waiting sign-in. The answer must come from the
// agent the question was put to, and is used once: a code that arrives late,
// twice, or from somewhere else finds nothing waiting for it.
func (s *signIn) deliver(owner tool.Owner, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.id == "" || s.delivered {
		return errNoPendingSignIn
	}
	if s.owner != owner {
		return errNotYourSignIn
	}
	s.delivered = true
	s.code <- code
	return nil
}

// askedOf reports whether this question was put to a given agent.
func (s *signIn) askedOf(owner tool.Owner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner == owner
}

// answered reports whether the question being asked has been answered, so a call
// arriving in the moment between the two is told to wait rather than asked for a
// second code.
func (s *signIn) answered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivered
}

// question is what the assistant has to be told, when the sign-in stopped to ask.
func (s *signIn) question() *needsCode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &needsCode{ID: s.id, Prompt: s.prompt, Instruction: s.instruction, Expires: s.expires}
}

func (s *signIn) finish(client *ssh.Client, err error) {
	s.mu.Lock()
	s.client, s.err = client, err
	s.mu.Unlock()
	close(s.done)
}

func (s *signIn) result() (*ssh.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client, s.err
}

// answersFirstQuestion decides whether the first thing a server asks is the
// password this tool holds.
//
// Three things have to hold, and none of them reads the question. There has to
// be a password. The server must not have taken it directly, because one that
// does has already had it and is now asking for something else. And there must
// be no key, because a tool signing in with a key is signing in with the key:
// its password, if it carries one, is not what a further question is for, and
// posting it into a prompt that wanted a verification code would send the
// account password somewhere it was never meant to go.
func (c Config) answersFirstQuestion(offered *passwordOffered) bool {
	return c.Auth.Password != "" &&
		strings.TrimSpace(c.Auth.PrivateKey) == "" &&
		!offered.wasTaken()
}

func newCodeID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("the sign-in could not be identified: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// refuseVerification is the challenge used where there is nobody to ask: the
// plain Test path. The password is still answered by the tool, because that is
// never a person's to give; anything beyond it stops the test, and reaching that
// point says the address, the host key and the credential were all accepted.
func refuseVerification(cfg Config, offered *passwordOffered) ssh.KeyboardInteractiveChallenge {
	return func(_, _ string, questions []string, _ []bool) ([]string, error) {
		if len(questions) == 0 {
			return nil, nil
		}
		answers := make([]string, len(questions))
		first := 0
		if cfg.Auth.Password != "" && !offered.wasTaken() {
			answers[0] = cfg.Auth.Password
			first = 1
		}
		if len(questions) == first {
			return answers, nil
		}
		return nil, errVerificationNeeded
	}
}
