package up

import (
	"io/ioutil"
	"os"

	"github.com/pkg/errors"
)

// GetChecksum from a given filepath. The file should be created on deploy and
// contain a sha256 checksum at time of deploy, calculated by up. It's used to
// determine whether another deploy is needed or redundant following a
// successful health check.
func GetChecksum(filepath string) ([]byte, error) {
	fi, err := os.Open("checksum")
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
