package up

import "testing"

func TestParse(t *testing.T) {
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
		}},
	}
	for _, tc := range tests {
		tc.Run(tc.haveFile, func(t *testing.T) {
			pth := filepath.Join("testdata", tc.haveFile)
			byt, err := ioutil.ReadAll(pth)
			if err != nil {
				t.Fatal(err)
			}
			rdr := bytes.NewReader(byt)
			conf, err := Parse(rdr)
			if err != nil {
				if tc.wantErr {
					continue
				}
				t.Fatal(err)
			}
			if !reflect.DeepEqual(conf, tc.want) {
				t.Fatalf("expected %+v, got %+v", tc.want, conf)
			}
		})
	}
}
