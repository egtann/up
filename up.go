package up

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
)

type CmdName string

// Config represents a parsed Upfile.
type Config struct {
	// Commands available to run grouped by command name.
	Commands map[CmdName]*Cmd

	// DefaultCommand is the first command in the Upfile.
	DefaultCommand CmdName

	// DefaultEnvironment is the first inventory in the Upfile.
	DefaultEnvironment string

	lex      *lexer
	text     string
	indented bool
}

// Cmd to run conditionally if the conditions listed in ExecIf all exit with
// zero.
type Cmd struct {
	// ExecIfs any of the following commands exit with non-zero codes.
	ExecIfs []CmdName

	// Execs these commands in order using the default shell.
	Execs []string
}

func ParseUpfile(rdr io.Reader) (*Config, error) {
	byt, err := ioutil.ReadAll(rdr)
	if err != nil {
		return nil, fmt.Errorf("read all: %w", err)
	}
	return parseUpfile(string(byt))
}

// GetCalculatedChecksum from a file which was created on deploy and contains
// only a sha256 checksum, calculated by up. This is an optional helper, but if
// used it can determine whether another deploy is needed or redundant
// following a successful health check.
func GetCalculatedChecksum(filepath string) ([]byte, error) {
	fi, err := os.Open(filepath)
	if os.IsNotExist(err) {
		return []byte{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer fi.Close()
	byt, err := ioutil.ReadAll(fi)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return byt, nil
}
