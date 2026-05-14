package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

func TestPublicURLAddress_UsesConfiguredHostAndResolvedPort(t *testing.T) {
	got := cli.PublicURLAddress("0.0.0.0:0", "127.0.0.1:54321")
	if got != "0.0.0.0:54321" {
		t.Fatalf("unexpected public address: got=%q want=%q", got, "0.0.0.0:54321")
	}
}

func TestPublicURLAddress_UsesConfiguredPortWhenResolvedPortUnavailable(t *testing.T) {
	got := cli.PublicURLAddress("0.0.0.0:0", "listener-address")
	if got != "0.0.0.0:0" {
		t.Fatalf("unexpected fallback address: got=%q want=%q", got, "0.0.0.0:0")
	}
}

func TestPublicURLAddress_ExtractsTrailingPortFromMalformedResolvedAddress(t *testing.T) {
	got := cli.PublicURLAddress("0.0.0.0:0", "bound on port:61234")
	if got != "0.0.0.0:61234" {
		t.Fatalf("unexpected recovered address: got=%q want=%q", got, "0.0.0.0:61234")
	}
}

func TestPublicURLAddress_FallsBackToConfiguredWhenResolvedEmpty(t *testing.T) {
	got := cli.PublicURLAddress("0.0.0.0:12345", "")
	if got != "0.0.0.0:12345" {
		t.Fatalf("unexpected configured fallback: got=%q want=%q", got, "0.0.0.0:12345")
	}
}

func TestPublicURLAddress_UsesDefaultWhenBothAddressesEmpty(t *testing.T) {
	got := cli.PublicURLAddress("", "")
	if got != "0.0.0.0:0" {
		t.Fatalf("unexpected default fallback: got=%q want=%q", got, "0.0.0.0:0")
	}
}

func TestExtractPortFromAddress(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{name: "host port", addr: "127.0.0.1:7000", want: "7000"},
		{name: "ipv6 host port", addr: "[::1]:7001", want: "7001"},
		{name: "trailing token", addr: "bound:7002", want: "7002"},
		{name: "invalid token", addr: "bound:abc", want: ""},
		{name: "empty", addr: "", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cli.ExtractPortFromAddress(tc.addr)
			if got != tc.want {
				t.Fatalf("unexpected extracted port: got=%q want=%q", got, tc.want)
			}
		})
	}
}
