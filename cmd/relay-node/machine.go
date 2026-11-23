package main

import (
	"context"
	"net"
	"net/url"
	"os"
	"time"
)

// Machine metadata is advertised at startup, without changing the saved Node
// configuration or replacing user labels. Prefer the interface used to reach
// Relay; interface order alone can select a Docker bridge or a VPN address.
func machineLabels(existing map[string]string, server string) map[string]string {
	labels := make(map[string]string, len(existing)+2)
	for key, value := range existing {
		labels[key] = value
	}
	if hostname, err := os.Hostname(); err == nil && labels["relay.node.hostname"] == "" {
		labels["relay.node.hostname"] = hostname
	}
	if usableMachineIP(net.ParseIP(labels["relay.node.ip"])) {
		return labels
	}
	var addresses []net.IP
	interfaces, _ := net.Interfaces()
	for _, item := range interfaces {
		if item.Flags&net.FlagUp == 0 || item.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := item.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil {
				addresses = append(addresses, ip)
			}
		}
	}
	if ip := selectMachineIP(relayRouteIP(server), addresses); ip != "" {
		labels["relay.node.ip"] = ip
	}
	return labels
}

func relayRouteIP(server string) net.IP {
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// UDP connect selects the local route without sending traffic to the server.
	connection, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil
	}
	defer connection.Close()
	if address, ok := connection.LocalAddr().(*net.UDPAddr); ok {
		return address.IP
	}
	return nil
}

func usableMachineIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}

func selectMachineIP(route net.IP, addresses []net.IP) string {
	if usableMachineIP(route) {
		return route.String()
	}
	for _, private := range []bool{true, false} {
		for _, ip := range addresses {
			if usableMachineIP(ip) && ip.To4() != nil && ip.IsPrivate() == private {
				return ip.String()
			}
		}
	}
	for _, ip := range addresses {
		if usableMachineIP(ip) {
			return ip.String()
		}
	}
	return ""
}
