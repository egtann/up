package up

import (
	"errors"
	"fmt"
)

// parseUpfile to build a Config tree.
func parseUpfile(text string) (*Config, error) {
	t := &Config{
		Commands: map[CmdName]*Cmd{},
		text:     text,
		lex:      lex(text),
	}
	if err := t.parse(); err != nil {
		t.lex.drain()
		t.stopParse()
		return nil, err
	}
	t.stopParse()

	// Validate to ensure that ExecIfs are defined after fully loading
	// them, since we don't require them to be defined in a specific order
	for cmdName, cmd := range t.Commands {
		for _, execIf := range cmd.ExecIfs {
			if execIf == cmdName {
				return nil, fmt.Errorf("%s depends on itself", execIf)
			}
			if _, exist := t.Commands[execIf]; !exist {
				return nil, fmt.Errorf("%s is undefined", execIf)
			}
		}
	}
	if len(t.Commands) == 0 {
		return nil, errors.New("no commands")
	}
	return t, nil
}

func (t *Config) parse() error {
	return t.nextControl(t.nextNonSpace())
}

func (t *Config) stopParse() {
	t.lex = nil
}

func (t *Config) nextNonSpace() token {
	for {
		tkn := t.lex.nextToken()
		if tkn.typ != tokenSpace {
			return tkn
		}
	}
}

func (t *Config) nextControl(tkn token) error {
	switch tkn.typ {
	case tokenEOF:
		return nil
	default:
		return t.commandControl(CmdName(tkn.val))
	}
}

func (t *Config) commandControl(name CmdName) error {
	if len(t.Commands) == 0 {
		t.DefaultCommand = name
	}
	if t.Commands[name] != nil {
		return fmt.Errorf("duplicate command %s", name)
	}
	cmd := Cmd{}

	// Get all tokenText until newline, ignoring non-newline spaces
Outer2:
	for {
		tkn := t.lex.nextToken()
		switch tkn.typ {
		case tokenText:
			cmd.ExecIfs = append(cmd.ExecIfs, CmdName(tkn.val))
		case tokenNewline:
			break Outer2
		case tokenSpace:
			// Do nothing
		case tokenEOF:
			return errors.New("unexpected eof in command line")
		default:
			return fmt.Errorf("unexpected command token %s (%d)", tkn.val, tkn.typ)
		}
	}

	// Get all tokenText until not indented
	var indented bool
	var line string
	var tkn token
Outer:
	for {
		tkn = t.lex.nextToken()
		switch tkn.typ {
		case tokenComment:
			skipLine(t.lex)
			indented = false
			continue
		case tokenNewline:
			indented = false
			if line != "" {
				cmd.Execs = append(cmd.Execs, line)
				line = ""
			}
			continue
		case tokenTab:
			if indented {
				if t.lex.nextToken().typ == tokenNewline {
					t.lex.backup()
					// Ignore extra whitespace at end of lines
					continue
				}
				// But error if there are too many tabs
				// otherwise
				return errors.New("unexpected double indent")
			}
			indented = true
			continue
		case tokenText, tokenSpace:
			if !indented {
				break Outer
			}
			// Continue parsing til the end of the line
			line += tkn.val
		case tokenEOF:
			break Outer
		default:
			return fmt.Errorf("unexpected %d %q", tkn.typ, tkn.val)
		}
	}

	// Ensure we found at least one
	if len(cmd.Execs) == 0 {
		return fmt.Errorf("nothing to exec for %s", name)
	}
	t.Commands[name] = &cmd
	if t.DefaultCommand == "" {
		t.DefaultCommand = name
	}
	return t.nextControl(tkn)
}

func skipLine(l *lexer) {
	for {
		tkn := l.nextToken()
		switch tkn.typ {
		case tokenNewline, tokenEOF:
			return
		default:
			continue
		}
	}
}
