package main

import (
	"fmt"
	"log"
	"testing"

	"git.sr.ht/~egtann/up"
)

func TestMakeBatches(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		serial int
		have   map[up.InvName][]string
		want   batch
	}{
		{
			serial: 1,
			have: map[up.InvName][]string{
				"srv1": []string{"a", "b", "c"},
			},
			want: batch{
				"srv1": [][]string{{"a"}, {"b"}, {"c"}},
			},
		},
		{
			serial: 3,
			have: map[up.InvName][]string{
				"srv1": []string{"a", "b", "c"},
				"srv2": []string{"d", "e"},
			},
			want: batch{
				"srv1": [][]string{{"a", "b", "c"}},
				"srv2": [][]string{{"d", "e"}},
			},
		},
		{
			serial: 0,
			have: map[up.InvName][]string{
				"srv1": []string{"a", "b", "c"},
				"srv2": []string{"d", "e"},
			},
			want: batch{
				"srv1": [][]string{{"a", "b", "c"}},
				"srv2": [][]string{{"d", "e"}},
			},
		},
		{
			serial: 2,
			have: map[up.InvName][]string{
				"srv1": []string{"a", "b", "c"},
				"srv2": []string{"d", "e", "f", "g"},
			},
			want: batch{
				"srv1": [][]string{{"a", "b"}, {"c"}},
				"srv2": [][]string{{"d", "e"}, {"f", "g"}},
			},
		},
		{
			serial: 3,
			have: map[up.InvName][]string{
				"srv1": []string{"a", "b", "c"},
				"srv2": []string{"d", "e", "f", "g"},
			},
			want: batch{
				"srv1": [][]string{{"a", "b", "c"}},
				"srv2": [][]string{{"d", "e", "f"}, {"g"}},
			},
		},
		{
			serial: 10,
			have: map[up.InvName][]string{
				"srv1": []string{"a", "b", "c"},
				"srv2": []string{"d", "e", "f", "g"},
			},
			want: batch{
				"srv1": [][]string{{"a", "b", "c"}},
				"srv2": [][]string{{"d", "e", "f", "g"}},
			},
		},
		{
			serial: 2,
			have: map[up.InvName][]string{
				"srv1": []string{"a", "b", "c"},
				"srv2": []string{"d", "e", "f", "g"},
				"srv3": []string{"d", "e"},
				"srv4": []string{"h", "j"},
				"srv5": []string{"k", "i"},
				"srv6": []string{"l", "m", "n"},
				"srv7": []string{"o"},
				"srv8": []string{"p", "q", "r", "s", "t", "u", "v"},
			},
			want: batch{
				"srv1": [][]string{{"a", "b"}, {"c"}},
				"srv2": [][]string{{"d", "e"}, {"f", "g"}},
				"srv3": [][]string{{"d", "e"}},
				"srv4": [][]string{{"h", "j"}},
				"srv5": [][]string{{"k", "i"}},
				"srv6": [][]string{{"l", "m"}, {"n"}},
				"srv7": [][]string{{"o"}},
				"srv8": [][]string{{"p", "q"}, {"r", "s"}, {"t", "u"}, {"v"}},
			},
		},
	}
	for i, tc := range tcs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			conf := &up.Config{Inventory: tc.have}
			batches, err := makeBatches(conf, tc.serial)
			if err != nil {
				t.Fatal(err)
			}
			for typ, ipgroups := range batches {
				wantgroups := tc.want[typ]
				if !sliceDeepEq(wantgroups, ipgroups) {
					log.Printf("%+v\n", batches)
					t.Fatalf("expected %+v, got %+v",
						tc.want[typ], ipgroups)
				}
			}
		})
	}
}

// sliceDeepEq compares nested slice equality without caring about order.
func sliceDeepEq(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	count := 0
	seen := map[string]struct{}{}
	for i, vs := range a {
		if len(vs) != len(b[i]) {
			return false
		}
		for _, v := range vs {
			count++
			seen[v] = struct{}{}
		}
	}
	for _, vs := range b {
		for _, v := range vs {
			seen[v] = struct{}{}
		}
	}
	return len(seen) == count
}
