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
		text: text,
		lex:  lex(text),
	}
	log.Println("parsing")
	if err := t.parse(); err != nil {
		t.lex.drain()
		t.stopParse()
		return nil, err
	}
	t.stopParse()
	return t, nil
}

func (t *Config) parse() error {
	tkn := t.nextNonSpace()
	switch tkn.typ {
	case tokenInventory:
		return t.inventoryControl()
	case tokenText:
		return t.commandControl(CmdName(tkn.val))
	}
	return errors.New("expected if")
}

func (t *Config) stopParse() {
	t.lex = nil
}

func (t *Config) nextNonSpace() token {
	var tkn token
	for {
		tkn = t.lex.nextToken()
		log.Println("next non space", tkn)
		if tkn.typ != tokenSpace {
			break
		}
	}
	return tkn
}

func (t *Config) inventoryControl() error {
	/*
		inventory production
			1.1.1.1
			1.1.2.1
		inventory staging
			1.1.2.2
	*/
	log.Println("inventoryControl")
	if t.Inventory == nil {
		t.Inventory = map[InvName][]string{}
	}
	tkn := t.nextNonSpace()
	curInvName := InvName(tkn.val)
	log.Println("USING", curInvName)
	inv := []string{}

	tkn = t.nextNonSpace()
	if tkn.typ != tokenNewline {
		return errors.New("expected newline")
	}

	// For each of the things that follow until a newline
	for tkn = t.nextNonSpace(); tkn.typ != tokenNewline; tkn = t.nextNonSpace() {
		tkn := tkn
		switch tkn.typ {
		case tokenText:
			if !t.indented {
				log.Println("not indented at token", tkn.val)
				break
			}
			inv = append(inv, tkn.val)
		default:
			return fmt.Errorf("unexpected %s", tkn.val)
		}
	}
	if len(inv) == 0 {
		return errors.New("empty inventory")
	}
	t.Inventory[curInvName] = inv
	return nil
}

func (t *Config) commandControl(name CmdName) error {
	log.Println("command control")
	if t.Commands[name] != nil {
		return fmt.Errorf("duplicate command %s", name)
	}
	cmd := Cmd{}

	for tkn := t.nextNonSpace(); tkn.typ != tokenNewline; tkn = t.nextNonSpace() {
		tkn := tkn
		switch tkn.typ {
		case tokenText:
			if !t.indented {
				break
			}
			cmd.ExecIfs = append(cmd.ExecIfs, CmdName(tkn.val))
		default:
			return fmt.Errorf("unexpected %s", tkn.val)
		}
	}

	// Ensure we found at least one
	if len(cmd.Execs) == 0 {
		return fmt.Errorf("nothing to exec for %s", name)
	}
	t.Commands[name] = &cmd
	return nil
}
