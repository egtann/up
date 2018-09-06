package up

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const eof = -1

type tokenType int

const (
	tokenError   tokenType = iota // Error occurred. Value is text of err
	tokenEOF                      // Designate the end of the file
	tokenSpace                    // Run of spaces separating arguments
	tokenTab                      // Tab '\t'
	tokenNewline                  // Line break
	tokenText                     // Plaintext
	tokenComment                  // Pound '#'

	// Keywords follow
	tokenKeyword   // Used only to delimit keywords
	tokenInventory // "inventory"
)

type token struct {
	typ tokenType
	pos int
	val string
}

type stateFn func(*lexer) stateFn

// run lexes the input by executing state functions until the state is nil.
func (l *lexer) run() {
	for l.state = lexText; l.state != nil; {
		l.state = l.state(l)
	}
	close(l.tokens) // No more tokens will be delivered
}

// lexer holds the state of the scanner.
type lexer struct {
	input   string     // The string being scanned
	state   stateFn    // The next lexing function to enter
	start   int        // Start position of this token
	pos     int        // Current position in the input
	width   int        // Width of the last rune read
	lastPos int        // Position of last token returned by nextToken
	tokens  chan token // Channel of scanned tokens
}

func lex(input string) *lexer {
	l := &lexer{
		input:  input,
		state:  lexText,
		tokens: make(chan token),
	}
	go l.run()
	return l
}

// drain the output so the lexing goroutine will exit. Called by the parser,
// not in the lexing goroutine.
func (l *lexer) drain() {
	for range l.tokens {
	}
}

// emit passes an token back to the client.
func (l *lexer) emit(t tokenType) {
	tkn := token{typ: t, val: l.input[l.start:l.pos]}
	l.tokens <- tkn
	l.start = l.pos
}

func (l *lexer) next() rune {
	if l.pos >= len(l.input) {
		l.width = 0
		return eof
	}
	r, w := utf8.DecodeRuneInString(l.input[l.pos:])
	l.width = w
	l.pos += l.width
	return r
}

// nextToken reports the next token from the input.
func (l *lexer) nextToken() token {
	token := <-l.tokens
	l.lastPos = token.pos
	return token
}

// ignore skips over the pending input before this point.
func (l *lexer) ignore() {
	l.start = l.pos
}

// backup steps back one rune. It can be called only once per call of next.
func (l *lexer) backup() {
	l.pos -= l.width
}

// peek returns but does not consume the next rune in the input.
func (l *lexer) peek() rune {
	r := l.next()
	l.backup()
	return r
}

// accept consumes the next rune if it's from the valid set.
func (l *lexer) accept(valid string) bool {
	if strings.IndexRune(valid, l.next()) >= 0 {
		return true
	}
	l.backup()
	return false
}

// acceptRun consumes a run of runes from the valid set.
func (l *lexer) acceptRun(valid string) {
	for strings.IndexRune(valid, l.next()) >= 0 {
	}
	l.backup()
}

// errorf returns an error token and terminates the scan by passing back a nil
// pointer as the next state, terminating l.run.
func (l *lexer) errorf(format string, args ...interface{}) stateFn {
	l.tokens <- token{typ: tokenError, val: fmt.Sprintf(format, args...)}
	return nil
}

func lexText(l *lexer) stateFn {
Outer:
	for {
		text := l.input[l.start:l.pos]
		r := l.next()
		switch {
		case r == eof:
			break Outer
		case r == '#':
			l.emit(tokenComment)
		case text == "inventory":
			l.backup()
			l.emit(tokenInventory)
		case isEndOfLine(r):
			l.backup()
			if len(text) > 0 {
				l.emit(tokenText)
			}
			l.next()
			l.emit(tokenNewline)
		case r == ' ':
			l.backup()
			if len(text) > 0 {
				l.emit(tokenText)
			}
			return lexSpace
		case r == '\t':
			l.emit(tokenTab)
		}
	}
	// Correctly reached EOF
	if l.pos > l.start {
		l.emit(tokenText)
	}
	l.emit(tokenEOF)
	return nil
}

func lexSpace(l *lexer) stateFn {
	for l.peek() == ' ' {
		l.next()
	}
	l.emit(tokenSpace)
	return lexText
}

func isAlphaNumeric(r rune) bool {
	return r == '_' || r == '.' || unicode.IsLetter(r) ||
		unicode.IsDigit(r)
}

func isEndOfLine(r rune) bool {
	return r == '\r' || r == '\n'
}
