package up

import (
	"encoding/json"
	"fmt"
	"io"
)

type Inventory map[string][]string

func ParseInventory(rdr io.Reader) (Inventory, error) {
	inv := Inventory{}
	if err := json.NewDecoder(rdr).Decode(&inv); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return inv, nil
}
