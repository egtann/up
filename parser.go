package up

import (
	"errors"
	"fmt"
	"log"
)

// parse free-form input to build a Config tree reflecting an Upfile's
// configuration.
func parse(text string) (*Config, error) {
	t := &Config{
		Commands:  map[CmdName]*Cmd{},
		Inventory: map[InvName][]string{},
		text:      text,
		lex:       lex(text),
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

func (t *Config) inventoryControl() error {
	/*
		inventory production
			1.1.1.1
			1.1.2.1
		inventory staging
			1.1.2.2
	*/
	tkn := t.nextNonSpace()
	curInvName := InvName(tkn.val)
	inv := []string{}

	tkn = t.nextNonSpace()
	if tkn.typ != tokenNewline {
		return errors.New("expected newline")
	}

	// For each of the things that follow until a newline
	var indented bool
Outer:
	for {
		tkn = t.lex.nextToken()
		switch tkn.typ {
		case tokenNewline:
			indented = false
			continue
		case tokenTab:
			if indented {
				return errors.New("unexpected double indent")
			}
			indented = true
			continue
		case tokenText:
			if !indented {
				break Outer
			}
			inv = append(inv, tkn.val)
		case tokenInventory:
			break Outer
		default:
			return fmt.Errorf("unexpected 1 %s", tkn.val)
		}
	}
	if len(inv) == 0 {
		return errors.New("empty inventory")
	}
	t.Inventory[curInvName] = inv
	return t.nextControl(tkn)
}

func (t *Config) nextControl(tkn token) error {
	switch tkn.typ {
	case tokenInventory:
		return t.inventoryControl()
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
		log.Println("tkn", tkn)
		switch tkn.typ {
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
	return t.nextControl(tkn)
}
