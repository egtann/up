package main

import (
	"fmt"
	"log"
	"testing"
)

func TestMakeBatches(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		have map[serviceType]*serviceConfig
		want map[serviceType][][]string
	}{
		{
			have: map[serviceType]*serviceConfig{
				"srv1": &serviceConfig{
					IPs:    []string{"a", "b", "c"},
					Serial: 1,
				},
			},
			want: map[serviceType][][]string{
				"srv1": [][]string{{"a"}, {"b"}, {"c"}},
			},
		},
		{
			have: map[serviceType]*serviceConfig{
				"srv1": &serviceConfig{
					IPs:    []string{"a", "b", "c"},
					Serial: 1,
				},
				"srv2": &serviceConfig{
					IPs:    []string{"d", "e"},
					Serial: 3,
				},
			},
			want: map[serviceType][][]string{
				"srv1": [][]string{{"a"}, {"b"}, {"c"}},
				"srv2": [][]string{{"d", "e"}},
			},
		},
		{
			have: map[serviceType]*serviceConfig{
				"srv1": &serviceConfig{
					IPs:    []string{"a", "b", "c"},
					Serial: 0,
				},
				"srv2": &serviceConfig{
					IPs:    []string{"d", "e"},
					Serial: 2,
				},
			},
			want: map[serviceType][][]string{
				"srv1": [][]string{{"a", "b", "c"}},
				"srv2": [][]string{{"d", "e"}},
			},
		},
		{
			have: map[serviceType]*serviceConfig{
				"srv1": &serviceConfig{
					IPs:    []string{"a", "b", "c"},
					Serial: 2,
				},
				"srv2": &serviceConfig{
					IPs:    []string{"d", "e", "f", "g"},
					Serial: 3,
				},
			},
			want: map[serviceType][][]string{
				"srv1": [][]string{{"a", "b"}, {"c"}},
				"srv2": [][]string{{"d", "e", "f"}, {"g"}},
			},
		},
	}
	for i, tc := range tcs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			batches, err := makeBatches(tc.have)
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

func TestValidateLimits(t *testing.T) {
	t.Parallel()
	limits := map[serviceType]struct{}{
		"srv1": {},
		"srv2": {},
	}
	services := map[serviceType]*serviceConfig{
		"srv1": nil,
		"srv2": nil,
	}
	const env = "test"
	if err := validateLimits(limits, services, env); err != nil {
		t.Fatal(err)
	}
	services = map[serviceType]*serviceConfig{
		"srv1": nil,
	}
	if err := validateLimits(limits, services, env); err == nil {
		t.Fatal("expected error, got none")
	}
}

// sliceDeepEq compares nested slice equality without caring about order.
func sliceDeepEq(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, vs := range a {
		seen := map[string]struct{}{}
		for _, v := range vs {
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
