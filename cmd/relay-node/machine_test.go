package main

import (
	"net"
	"testing"
)

func TestMachineIPSelection(t *testing.T) {
	for _, test := range []struct {
		name, route string
		addresses   []string
		want        string
	}{
		{"route beats bridge", "10.37.82.77", []string{"172.17.0.1"}, "10.37.82.77"},
		{"loopback Relay", "127.0.0.1", []string{"127.0.0.1", "169.254.1.2", "10.37.82.77"}, "10.37.82.77"},
		{"prefer private IPv4", "", []string{"2001:db8::1", "8.8.8.8", "10.37.82.77"}, "10.37.82.77"},
		{"IPv6 route", "2001:db8::2", []string{"10.37.82.77"}, "2001:db8::2"},
		{"IPv6 only", "", []string{"fe80::1", "2001:db8::2"}, "2001:db8::2"},
		{"no usable address", "::1", []string{"0.0.0.0", "127.0.0.1", "fe80::1"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var addresses []net.IP
			for _, address := range test.addresses {
				addresses = append(addresses, net.ParseIP(address))
			}
			if got := selectMachineIP(net.ParseIP(test.route), addresses); got != test.want {
				t.Fatalf("IP=%q, want %q", got, test.want)
			}
		})
	}
}

func TestMachineLabelsPreserveExplicitConfiguration(t *testing.T) {
	original := map[string]string{"name": "Build machine", "relay.node.ip": "10.37.82.77", "custom": "value"}
	labels := machineLabels(original, "invalid")
	if labels["name"] != original["name"] || labels["custom"] != "value" || labels["relay.node.ip"] != "10.37.82.77" {
		t.Fatalf("user labels replaced: %+v", labels)
	}
	if _, changed := original["relay.node.hostname"]; changed {
		t.Fatal("mutated saved configuration labels")
	}
}
