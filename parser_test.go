package up

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		haveFile string
		want     *Config
		wantErr  bool
	}{
		{haveFile: "empty", wantErr: true},
		{haveFile: "dupe_inventory", wantErr: true},
		{haveFile: "invalid_inventory", wantErr: true},
		{haveFile: "two_inventory_groups", want: &Config{
			Inventory: map[InvName][]string{
				"production": []string{"1.1.1.1"},
				"staging":    []string{"www.example.com", "1.1.1.2"},
			},
			Commands: map[CmdName]*Cmd{
				"deploy": &Cmd{
					ExecIfs: []CmdName{"if1"},
					Execs:   []string{"echo 'hello world'"},
				},
				"if1": &Cmd{Execs: []string{"echo 'if1'"}},
			},
			DefaultCommand:     "deploy",
			DefaultEnvironment: "production",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.haveFile, func(t *testing.T) {
			pth := filepath.Join("testdata", tc.haveFile)
			byt, err := ioutil.ReadFile(pth)
			if err != nil {
				t.Fatal(err)
			}
			rdr := bytes.NewReader(byt)
			conf, err := Parse(rdr)
			if err != nil {
				if tc.wantErr {
					return
				}
				t.Fatal(err)
			}
			byt, err = json.Marshal(conf)
			if err != nil {
				t.Fatal(err)
			}
			got := string(byt)
			byt, err = json.Marshal(tc.want)
			if err != nil {
				t.Fatal(err)
			}
			want := string(byt)
			if got != want {
				t.Fatalf("expected: %s\ngot: %s", want, got)
			}
		})
	}
}
