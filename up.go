package up

import (
	"io"
	"io/ioutil"
	"os"

	"github.com/pkg/errors"
)

type InvName string
type CmdName string

// Config represents a parsed Upfile.
type Config struct {
	// Inventory of IPs or URLs associated by named groups.
	Inventory map[InvName][]string

	// Commands available to run grouped by command name.
	Commands map[CmdName]*Cmd

	// DefaultCommand is the first command in the Upfile.
	DefaultCommand CmdName

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

func Parse(rdr io.Reader) (*Config, error) {
	byt, err := ioutil.ReadAll(rdr)
	if err != nil {
		return nil, errors.Wrap(err, "read all")
	}
	return parse(string(byt))
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
		return nil, errors.Wrap(err, "open file")
	}
	defer fi.Close()
	byt, err := ioutil.ReadAll(fi)
	if err != nil {
		return nil, errors.Wrap(err, "read file")
	}
	return byt, nil
}
