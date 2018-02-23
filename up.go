package up

import (
	"io/ioutil"
	"os"

	"github.com/pkg/errors"
)

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
